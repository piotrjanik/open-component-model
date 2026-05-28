package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ociImageSpecV1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content"
	orasregistry "oras.land/oras-go/v2/registry"
	"sigs.k8s.io/yaml"

	"ocm.software/open-component-model/bindings/go/blob"
	"ocm.software/open-component-model/bindings/go/blob/inmemory"
	"ocm.software/open-component-model/bindings/go/constructor"
	constructorruntime "ocm.software/open-component-model/bindings/go/constructor/runtime"
	constructorv1 "ocm.software/open-component-model/bindings/go/constructor/spec/v1"
	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	"ocm.software/open-component-model/bindings/go/input/file"
	"ocm.software/open-component-model/bindings/go/oci"
	"ocm.software/open-component-model/bindings/go/oci/looseref"
	urlresolver "ocm.software/open-component-model/bindings/go/oci/resolver/url"
	v1 "ocm.software/open-component-model/bindings/go/oci/spec/access/v1"
	"ocm.software/open-component-model/bindings/go/oci/spec/annotations"
	"ocm.software/open-component-model/bindings/go/oci/spec/layout"
	"ocm.software/open-component-model/bindings/go/repository"
	ocmruntime "ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/cli/cmd"
	"ocm.software/open-component-model/cli/integration/internal"
)

// Test_Integration_AssetToOwner verifies the asset-to-owner scenario end-to-end
// (ADR 0016) against live containerised registries, covering both halves of the
// feature as nested subtests:
//
//   - "ocm add cv": a component-constructor YAML — the same artifact the add cv
//     command consumes — is run through the real constructor engine
//     (constructor.NewDefaultConstructor) against a live registry. A resource that
//     opts in via ownershipPolicy=Always becomes discoverable as an ownership
//     referrer, both for by-value "input" resources and for by-reference
//     "imageReference" resources. Resources that do not opt in get none.
//   - "ocm transfer": exercises the real `ocm transfer component-version` CLI
//     command. A by-value resource that carries an ownership referrer in the
//     source has it copied onto the target after transfer, while a by-reference
//     resource that carries none transfers none.
//
// The add cv coverage uses two drivers on purpose: most subtests drive the
// constructor engine directly (constructor.NewDefaultConstructor) to exercise the
// policy gate in constructor.processResource across resource shapes, while a
// dedicated subtest drives the real `ocm add component-version` command to cover
// the wired CLI seam (GetResourceRepository -> constructorPlugin.AddOwnershipReferrer).
// The transfer half likewise runs the real CLI command.
//
// Verification always goes through the OCI Distribution Referrers API
// (`registry.Referrers`) — the same path every OCI v1.1 client uses.
func Test_Integration_AssetToOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const version = "v1.0.0"

	t.Run("ocm add cv", func(t *testing.T) {
		const component = "ocm.software/test-asset"

		// The source registry also hosts the images for the by-reference resources.
		srcResolver, srcReg := ownershipRegistry(t)
		srcAddr := srcReg.RegistryAddress
		srcRepo := newOwnershipRepository(t, srcResolver)

		// One component version is constructed from a single component-constructor
		// YAML carrying three resources that differ only in how each opts into
		// ownership (ADR 0016) via options.ownershipPolicy — never via relation:
		//   - backend-image-local: a by-value file/v1 input (an OCI image layout
		//     tarball), ownershipPolicy=Always        → referrer created
		//   - backend-image: a by-reference image, ownershipPolicy=Always → referrer created
		//   - backend-image-external: a by-reference image, ownershipPolicy=Never → none
		// The two by-reference resources point at distinct images so each subject's
		// referrers can be asserted independently.
		workingDir := t.TempDir()
		writeOCILayoutTarball(t, workingDir, "hello-ocm.tar.gz", []byte("backend-image-local-payload"))

		ownedImageRef := pushByReferenceImage(t, ctx, srcRepo, "backend-image", version,
			fmt.Sprintf("%s/test-asset/backend-image:%s", srcAddr, version), []byte("backend-image-payload"))
		externalImageRef := pushByReferenceImage(t, ctx, srcRepo, "backend-image-external", version,
			fmt.Sprintf("%s/test-asset/backend-image-external:%s", srcAddr, version), []byte("backend-image-external-payload"))

		constructorYAML := fmt.Sprintf(`
components:
  - name: %[1]s
    version: %[2]s
    provider:
      name: ocm.software
    resources:
      - name: backend-image-local
        version: %[2]s
        type: ociArtifact
        options:
          ownershipPolicy: Always
        input:
          type: file/v1
          path: hello-ocm.tar.gz
          mediaType: application/vnd.ocm.software.oci.layout.v1+tar+gzip
      - name: backend-image
        version: %[2]s
        type: ociArtifact
        options:
          ownershipPolicy: Always
        access:
          type: OCIImage/v1
          imageReference: %[3]s
      - name: backend-image-external
        version: %[2]s
        type: ociArtifact
        options:
          ownershipPolicy: Never
        access:
          type: OCIImage/v1
          imageReference: %[4]s
`, component, version, ownedImageRef, externalImageRef)

		addComponentVersion(t, ctx, srcRepo, workingDir, constructorYAML)

		t.Run("input resource (ownershipPolicy=Always) — ownership referrer is created", func(t *testing.T) {
			r := require.New(t)
			subjectRef := localResourceSubjectReference(t, ctx, srcResolver, srcRepo, component, version, "backend-image-local")
			referrers := listOwnershipReferrers(t, ctx, srcResolver, subjectRef)
			r.Len(referrers, 1, "a by-value input resource that opts in must get exactly one ownership referrer")
			assertOwnership(t, ctx, srcResolver, subjectRef, referrers[0], component, version, "backend-image-local")
		})

		t.Run("imageReference access (ownershipPolicy=Always) — ownership referrer is created", func(t *testing.T) {
			r := require.New(t)
			referrers := listOwnershipReferrers(t, ctx, srcResolver, ownedImageRef)
			r.Len(referrers, 1, "a by-reference resource that opts in must get exactly one ownership referrer on its image")
			assertOwnership(t, ctx, srcResolver, ownedImageRef, referrers[0], component, version, "backend-image")
		})

		t.Run("imageReference access (ownershipPolicy=Never) — ownership referrer is not created", func(t *testing.T) {
			r := require.New(t)
			r.Empty(listOwnershipReferrers(t, ctx, srcResolver, externalImageRef),
				"a resource that does not opt in must not get an ownership referrer")
		})
	})

	t.Run("ocm add cv via CLI — by-reference attach", func(t *testing.T) {
		// Unlike the engine-driven "ocm add cv" subtest above, this drives the real
		// `ocm add component-version` command so the production CLI seam is exercised
		// end to end: GetResourceRepository -> constructorPlugin.AddOwnershipReferrer.
		// A CLI-driven case here is what would catch a regression of the by-reference
		// attach.
		r := require.New(t)
		const component = "ocm.software/cli-asset"

		resolver, reg := ownershipRegistry(t)
		repo := newOwnershipRepository(t, resolver)

		ownedImageRef := pushByReferenceImage(t, ctx, repo, "backend-image", version,
			fmt.Sprintf("%s/cli-asset/backend-image:%s", reg.RegistryAddress, version), []byte("cli-backend-image-payload"))
		r.Empty(listOwnershipReferrers(t, ctx, resolver, ownedImageRef),
			"staging the image must not attach a referrer on its own")

		constructorYAML := fmt.Sprintf(`
components:
  - name: %[1]s
    version: %[2]s
    provider:
      name: ocm.software
    resources:
      - name: backend-image
        version: %[2]s
        type: ociArtifact
        options:
          ownershipPolicy: Always
        access:
          type: OCIImage/v1
          imageReference: %[3]s
`, component, version, ownedImageRef)
		constructorPath := filepath.Join(t.TempDir(), "constructor.yaml")
		r.NoError(os.WriteFile(constructorPath, []byte(constructorYAML), os.ModePerm))

		cfgPath, err := internal.CreateOCMConfigForRegistry(t, []internal.ConfigOpts{
			{Host: reg.Host, Port: reg.Port, User: reg.User, Password: reg.Password},
		})
		r.NoError(err)

		addCMD := cmd.New()
		out := new(bytes.Buffer)
		addCMD.SetOut(out)
		addCMD.SetErr(out)
		addCMD.SetArgs([]string{
			"add", "component-version",
			"--repository", fmt.Sprintf("http://%s", reg.RegistryAddress),
			"--constructor", constructorPath,
			"--config", cfgPath,
		})

		addCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		r.NoError(addCMD.ExecuteContext(addCtx), "add cv should succeed: %s", out.String())

		_, err = repo.GetComponentVersion(addCtx, component, version)
		r.NoError(err, "the component version must be added")
		referrers := listOwnershipReferrers(t, addCtx, resolver, ownedImageRef)
		r.Len(referrers, 1, "the real add cv command must attach exactly one ownership referrer to the by-reference image")
		assertOwnership(t, addCtx, resolver, ownedImageRef, referrers[0], component, version, "backend-image")
	})

	t.Run("ocm add cv — by-value create is idempotent (single referrer on re-add)", func(t *testing.T) {
		r := require.New(t)
		const (
			component    = "ocm.software/asset-to-owner/idempotent"
			resourceName = "backend-image"
		)
		resolver, _ := ownershipRegistry(t)
		repo := newOwnershipRepository(t, resolver)

		// The referrer is content-addressed off the subject, so adding the same
		// by-value resource twice must converge on exactly one referrer — not two.
		// Enumerating the live Referrers API (not Exists) is what proves "exactly one".
		addOwnershipResource(t, ctx, repo, component, version, resourceName, true)
		addOwnershipResource(t, ctx, repo, component, version, resourceName, true)

		subjectRef := localResourceSubjectReference(t, ctx, resolver, repo, component, version, resourceName)
		referrers := listOwnershipReferrers(t, ctx, resolver, subjectRef)
		r.Len(referrers, 1, "re-adding the same by-value resource must leave exactly one ownership referrer")
		assertOwnership(t, ctx, resolver, subjectRef, referrers[0], component, version, resourceName)
	})

	t.Run("ocm add cv — sibling resources get isolated referrers", func(t *testing.T) {
		r := require.New(t)
		const component = "ocm.software/asset-to-owner/siblings"
		resolver, _ := ownershipRegistry(t)
		repo := newOwnershipRepository(t, resolver)

		// Two by-value resources with distinct content (distinct subjects) in one
		// component version: each subject must carry exactly its own referrer, with
		// its own software.ocm.artifact identity — never the sibling's.
		mkRes := func(name string, payload []byte) descriptor.Resource {
			data, _ := createSingleLayerOCIImage(t, payload)
			res, err := repo.AddLocalResource(ctx, component, version, &descriptor.Resource{
				ElementMeta: descriptor.ElementMeta{ObjectMeta: descriptor.ObjectMeta{Name: name, Version: version}},
				Type:        "ociArtifact",
				Relation:    descriptor.LocalRelation,
				Access: &v2.LocalBlob{
					Type:           ocmruntime.Type{Name: v2.LocalBlobAccessType, Version: v2.LocalBlobAccessTypeVersion},
					MediaType:      layout.MediaTypeOCIImageLayoutTarGzipV1,
					LocalReference: digest.FromBytes(data).String(),
				},
			}, inmemory.New(bytes.NewReader(data)), repository.WithOwnershipReferrerCreation(true))
			r.NoError(err)
			return *res
		}
		backend := mkRes("backend", []byte("siblings-backend-payload"))
		frontend := mkRes("frontend", []byte("siblings-frontend-payload"))
		r.NoError(repo.AddComponentVersion(ctx, &descriptor.Descriptor{
			Meta: descriptor.Meta{Version: "v2"},
			Component: descriptor.Component{
				Provider:      descriptor.Provider{Name: "ocm.software"},
				ComponentMeta: descriptor.ComponentMeta{ObjectMeta: descriptor.ObjectMeta{Name: component, Version: version}},
				Resources:     []descriptor.Resource{backend, frontend},
			},
		}))

		backendSubject := localResourceSubjectReference(t, ctx, resolver, repo, component, version, "backend")
		frontendSubject := localResourceSubjectReference(t, ctx, resolver, repo, component, version, "frontend")
		r.NotEqual(backendSubject, frontendSubject, "sibling resources must have distinct subjects")

		backendRefs := listOwnershipReferrers(t, ctx, resolver, backendSubject)
		r.Len(backendRefs, 1, "backend must carry exactly its own referrer")
		assertOwnership(t, ctx, resolver, backendSubject, backendRefs[0], component, version, "backend")

		frontendRefs := listOwnershipReferrers(t, ctx, resolver, frontendSubject)
		r.Len(frontendRefs, 1, "frontend must carry exactly its own referrer")
		assertOwnership(t, ctx, resolver, frontendSubject, frontendRefs[0], component, version, "frontend")
	})

	t.Run("ocm transfer", func(t *testing.T) {
		// A by-value resource's ownership referrer travels inside the resource's local
		// blob layout, which a component-version transfer always carries — so the
		// referrer reaches the target whether or not resources are copied by value.
		// --copy-resources is exercised both ways to lock that in.
		for _, copyResources := range []bool{false, true} {
			name := "resource with referrer (ownershipPolicy=Always) — referrer is transferred without --copy-resources"
			if copyResources {
				name = "resource with referrer (ownershipPolicy=Always) — referrer is transferred with --copy-resources"
			}
			t.Run(name, func(t *testing.T) {
				r := require.New(t)
				const (
					component    = "ocm.software/asset-to-owner/transfer-byvalue"
					resourceName = "backend-image"
				)

				srcResolver, srcReg := ownershipRegistry(t)
				srcRepo := newOwnershipRepository(t, srcResolver)
				dstResolver, dstReg := ownershipRegistry(t)
				dstRepo := newOwnershipRepository(t, dstResolver)

				// Author a by-value resource on the source registry; opting in (createReferrer)
				// makes AddLocalResource also push one ownership referrer next to the uploaded
				// manifest. A component version is added so the resource is transferable.
				addOwnershipResource(t, ctx, srcRepo, component, version, resourceName, true)
				srcSubjectRef := localResourceSubjectReference(t, ctx, srcResolver, srcRepo, component, version, resourceName)
				r.Len(listOwnershipReferrers(t, ctx, srcResolver, srcSubjectRef), 1,
					"the source must carry the created ownership referrer")

				// The command needs credentials for both registries.
				cfgPath, err := internal.CreateOCMConfigForRegistry(t, []internal.ConfigOpts{
					{Host: srcReg.Host, Port: srcReg.Port, User: srcReg.User, Password: srcReg.Password},
					{Host: dstReg.Host, Port: dstReg.Port, User: dstReg.User, Password: dstReg.Password},
				})
				r.NoError(err)

				// Transfer source -> target registry. The ownership referrer travels inside
				// the resource's layout and is copied across (the re-add reads
				// ownershipPolicy=Never, since the policy is not persisted to the descriptor,
				// so the target only copies; it never re-creates).
				args := []string{
					"transfer", "component-version",
					fmt.Sprintf("http://%s//%s:%s", srcReg.RegistryAddress, component, version),
					fmt.Sprintf("http://%s", dstReg.RegistryAddress),
					"--config", cfgPath,
				}
				if copyResources {
					args = append(args, "--copy-resources")
				}

				transferCMD := cmd.New()
				transferOut := new(bytes.Buffer)
				transferCMD.SetOut(transferOut)
				transferCMD.SetErr(transferOut)
				transferCMD.SetArgs(args)

				transferCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				r.NoError(transferCMD.ExecuteContext(transferCtx), "transfer should succeed: %s", transferOut.String())

				// The referrer must be discoverable on the target — copied from the source.
				dstSubjectRef := localResourceSubjectReference(t, transferCtx, dstResolver, dstRepo, component, version, resourceName)
				dstReferrers := listOwnershipReferrers(t, transferCtx, dstResolver, dstSubjectRef)
				r.Len(dstReferrers, 1, "the ownership referrer must be present on the target registry after transfer")
				assertOwnership(t, transferCtx, dstResolver, dstSubjectRef, dstReferrers[0], component, version, resourceName)
			})
		}

		t.Run("resource missing referrer in source — referrer is not transferred", func(t *testing.T) {
			r := require.New(t)
			const resourceName = "backend-image"

			srcResolver, srcReg := ownershipRegistry(t)
			srcRepo := newOwnershipRepository(t, srcResolver)
			dstResolver, dstReg := ownershipRegistry(t)
			dstRepo := newOwnershipRepository(t, dstResolver)

			srcImageRef := fmt.Sprintf("%s/asset-to-owner/transfer-imageref:%s", srcReg.RegistryAddress, version)

			// Author on the source by reference, but DO NOT attach an ownership referrer —
			// the resource did not opt in, so the source image has none. The by-reference
			// referrer copy is a binding-level path the transfer CLI does not exercise, so
			// this half is driven directly against the repositories.
			data, access := createSingleLayerOCIImage(t, []byte("transfer-imageref-payload"), srcImageRef)
			srcRes, err := srcRepo.UploadResource(ctx, &descriptor.Resource{
				ElementMeta: descriptor.ElementMeta{ObjectMeta: descriptor.ObjectMeta{Name: resourceName, Version: version}},
				Type:        "ociArtifact",
				Access:      access,
			}, inmemory.New(bytes.NewReader(data)))
			r.NoError(err)
			r.Empty(listOwnershipReferrers(t, ctx, srcResolver, srcRes.Access.(*v1.OCIImage).ImageReference),
				"the source image must carry no ownership referrer for this scenario")

			// Transfer the by-reference image to the target registry. The by-reference
			// upload path only COPIES referrers that travel inside the layout — it never
			// re-creates them — and the source carried none, so the target must too.
			blobContent, err := srcRepo.DownloadResource(ctx, srcRes)
			r.NoError(err)
			transferRes := srcRes.DeepCopy()
			transferRes.Access.(*v1.OCIImage).ImageReference = fmt.Sprintf("%s/asset-to-owner/transfer-imageref:%s", dstReg.RegistryAddress, version)
			transferRes.Digest = nil

			dstRes, err := dstRepo.UploadResource(ctx, transferRes, blobContent)
			r.NoError(err)

			r.Empty(listOwnershipReferrers(t, ctx, dstResolver, dstRes.Access.(*v1.OCIImage).ImageReference),
				"a local imageReference resource missing its source referrer must transfer none")
		})
	})
}

// --- ocm add cv: driving the real constructor engine (ADR 0016) ---------------
//
// These helpers run a component-constructor YAML document — the same artifact
// `ocm add cv --constructor` consumes — through constructor.NewDefaultConstructor,
// wiring just enough providers to drive the engine directly against the live
// repository under test. This exercises the actual add cv code path (the policy
// gate in constructor.processResource) instead of mirroring it at the call site.
// A separate subtest drives the real `ocm add component-version` command to cover
// the wired CLI seam; these engine-driven helpers focus on the policy gate itself.

// parseConstructor unmarshals a component-constructor YAML document into the
// runtime constructor the engine operates on, exactly as the add cv command does.
func parseConstructor(t *testing.T, constructorYAML string) *constructorruntime.ComponentConstructor {
	t.Helper()
	var spec constructorv1.ComponentConstructor
	require.NoError(t, yaml.Unmarshal([]byte(constructorYAML), &spec))
	return constructorruntime.ConvertToRuntimeConstructor(&spec)
}

// addComponentVersion runs the constructor engine for constructorYAML against repo,
// resolving file/v1 inputs relative to workingDir. repo is both the construction
// target and — for resources that opt in and are kept by reference — the
// ownership-referrer host.
func addComponentVersion(t *testing.T, ctx context.Context, repo *oci.Repository, workingDir, constructorYAML string) {
	t.Helper()
	inputMethod, err := file.NewInputMethod(workingDir)
	require.NoError(t, err)
	c := constructor.NewDefaultConstructor(parseConstructor(t, constructorYAML), constructor.Options{
		TargetRepositoryProvider:       targetRepositoryProvider{repo},
		ResourceInputMethodProvider:    fileInputProvider{inputMethod},
		ResourceRepositoryProvider:     ownershipResourceRepositoryProvider{repo},
		ComponentVersionConflictPolicy: constructor.ComponentVersionConflictReplace,
	})
	require.NoError(t, c.Construct(ctx))
}

// pushByReferenceImage uploads a one-layer OCI image to imageRef as a by-reference
// resource carrying no ownership policy (so the push itself attaches no referrer)
// and returns the reference of the uploaded image for a constructor access to point
// at. add cv attaches a referrer to an existing image; it never pushes one.
func pushByReferenceImage(t *testing.T, ctx context.Context, repo *oci.Repository, resourceName, version, imageRef string, payload []byte) string {
	t.Helper()
	r := require.New(t)
	data, access := createSingleLayerOCIImage(t, payload, imageRef)
	uploaded, err := repo.UploadResource(ctx, &descriptor.Resource{
		ElementMeta: descriptor.ElementMeta{ObjectMeta: descriptor.ObjectMeta{Name: resourceName, Version: version}},
		Type:        "ociArtifact",
		Access:      access,
	}, inmemory.New(bytes.NewReader(data)))
	r.NoError(err)
	return uploaded.Access.(*v1.OCIImage).ImageReference
}

// localResourceSubjectReference reads the component version back and returns the full
// OCI reference (component-descriptors repo @ local-blob digest) of resourceName — the
// subject an ownership referrer of a by-value resource points at.
func localResourceSubjectReference(t *testing.T, ctx context.Context, resolver *urlresolver.CachingResolver, repo *oci.Repository, component, version, resourceName string) string {
	t.Helper()
	r := require.New(t)
	desc, err := repo.GetComponentVersion(ctx, component, version)
	r.NoError(err)
	for _, res := range desc.Component.Resources {
		if res.Name == resourceName {
			var local v2.LocalBlob
			r.NoError(v2.Scheme.Convert(res.Access, &local))
			ref, err := looseref.ParseReference(resolver.ComponentVersionReference(ctx, component, version))
			r.NoError(err)
			ref.Tag = ""
			ref.Reference.Reference = local.LocalReference
			return ref.String()
		}
	}
	t.Fatalf("resource %q not present in component version %s:%s", resourceName, component, version)
	return ""
}

var (
	_ constructor.TargetRepositoryProvider    = targetRepositoryProvider{}
	_ constructor.ResourceInputMethodProvider = fileInputProvider{}
	_ constructor.ResourceRepositoryProvider  = ownershipResourceRepositoryProvider{}
	_ constructor.ResourceRepository          = ownershipResourceRepository{}
	_ constructor.OwnershipReferrerAttacher   = ownershipResourceRepository{}
)

// targetRepositoryProvider hands the engine the repository under test as the
// construction target. *oci.Repository already satisfies constructor.TargetRepository.
type targetRepositoryProvider struct{ repo *oci.Repository }

func (p targetRepositoryProvider) GetTargetRepository(context.Context, *constructorruntime.Component) (constructor.TargetRepository, error) {
	return p.repo, nil
}

// fileInputProvider serves the real file/v1 input method (bindings/go/input/file),
// which reads each resource's input path relative to its working directory — the
// same method `ocm add cv` uses for file inputs.
type fileInputProvider struct{ method *file.InputMethod }

func (p fileInputProvider) GetResourceInputMethod(context.Context, *constructorruntime.Resource) (constructor.ResourceInputMethod, error) {
	return p.method, nil
}

// writeOCILayoutTarball writes a deterministic one-layer OCI image layout tarball
// (an application/vnd.ocm.software.oci.layout.v1+tar+gzip blob) to dir/name — the
// on-disk artifact a file/v1 input feeds into a by-value resource.
func writeOCILayoutTarball(t *testing.T, dir, name string, payload []byte) {
	t.Helper()
	data, _ := createSingleLayerOCIImage(t, payload)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
}

// ownershipResourceRepositoryProvider adapts *oci.Repository to the engine's
// resource-repository and ownership-referrer-attacher interfaces, which carry a
// credentials argument the repository methods omit. The engine calls
// AddOwnershipReferrer only for resources that opt in (ownershipPolicy=Always).
type ownershipResourceRepositoryProvider struct{ repo *oci.Repository }

func (p ownershipResourceRepositoryProvider) GetResourceRepository(context.Context, *constructorruntime.Resource) (constructor.ResourceRepository, error) {
	return ownershipResourceRepository{p.repo}, nil
}

type ownershipResourceRepository struct{ repo *oci.Repository }

func (ownershipResourceRepository) GetResourceCredentialConsumerIdentity(context.Context, *constructorruntime.Resource) (ocmruntime.Identity, error) {
	return nil, nil
}

func (r ownershipResourceRepository) DownloadResource(ctx context.Context, res *descriptor.Resource, _ ocmruntime.Typed) (blob.ReadOnlyBlob, error) {
	return r.repo.DownloadResource(ctx, res)
}

func (r ownershipResourceRepository) AddOwnershipReferrer(ctx context.Context, component, version string, res *descriptor.Resource, _ ocmruntime.Typed) error {
	return r.repo.AddOwnershipReferrer(ctx, component, version, res)
}

// --- shared helpers -----------------------------------------------------------

// addOwnershipResource authors a single by-value resource (an OCI image layout
// local blob) on repo and adds a component version holding it. When createReferrer
// is true the AddLocalResource pack also pushes one ownership referrer next to the
// uploaded manifest (via repository.WithOwnershipReferrerCreation), so the resource
// becomes a transfer source that carries the referrer.
func addOwnershipResource(t *testing.T, ctx context.Context, repo *oci.Repository, component, version, resourceName string, createReferrer bool) {
	t.Helper()
	r := require.New(t)

	data, _ := createSingleLayerOCIImage(t, []byte("transfer-byvalue-payload"))
	res, err := repo.AddLocalResource(ctx, component, version, &descriptor.Resource{
		ElementMeta: descriptor.ElementMeta{ObjectMeta: descriptor.ObjectMeta{Name: resourceName, Version: version}},
		Type:        "ociArtifact",
		Relation:    descriptor.LocalRelation,
		Access: &v2.LocalBlob{
			Type:           ocmruntime.Type{Name: v2.LocalBlobAccessType, Version: v2.LocalBlobAccessTypeVersion},
			MediaType:      layout.MediaTypeOCIImageLayoutTarGzipV1,
			LocalReference: digest.FromBytes(data).String(),
		},
	}, inmemory.New(bytes.NewReader(data)), repository.WithOwnershipReferrerCreation(createReferrer))
	r.NoError(err)
	r.NoError(repo.AddComponentVersion(ctx, &descriptor.Descriptor{
		Meta: descriptor.Meta{Version: "v2"},
		Component: descriptor.Component{
			Provider:      descriptor.Provider{Name: "ocm.software"},
			ComponentMeta: descriptor.ComponentMeta{ObjectMeta: descriptor.ObjectMeta{Name: component, Version: version}},
			Resources:     []descriptor.Resource{*res},
		},
	}))
}

// assertOwnership checks an ADR-0016 ownership referrer: its annotations and that
// its subject points at the subject manifest as it exists on this registry.
// subjectRef is the full OCI reference of the owned artifact (by tag or digest).
func assertOwnership(t *testing.T, ctx context.Context, resolver *urlresolver.CachingResolver, subjectRef string, ref ociImageSpecV1.Descriptor, component, version, resourceName string) {
	t.Helper()
	r := require.New(t)

	assert.Equal(t, component, ref.Annotations[annotations.OwnershipComponentName])
	assert.Equal(t, version, ref.Annotations[annotations.OwnershipComponentVersion])

	var payload struct {
		Identity map[string]string `json:"identity"`
		Kind     string            `json:"kind"`
	}
	r.NoError(json.Unmarshal([]byte(ref.Annotations[annotations.ArtifactAnnotationKey]), &payload))
	assert.Equal(t, "resource", payload.Kind)
	assert.Equal(t, resourceName, payload.Identity["name"])
	assert.Equal(t, version, payload.Identity["version"])

	// The Referrers API indexes by subject, so a referrer with a stale or wrong
	// subject digest would still be returned for this subject — assert the
	// referrer manifest's subject actually matches the resolved subject digest.
	store, err := resolver.StoreForReference(ctx, subjectRef)
	r.NoError(err)
	sref, err := looseref.ParseReference(subjectRef)
	r.NoError(err)
	subject, err := store.Resolve(ctx, sref.ReferenceOrTag())
	r.NoError(err)

	rc, err := store.Fetch(ctx, ref)
	r.NoError(err)
	defer func() { r.NoError(rc.Close()) }()
	var manifest ociImageSpecV1.Manifest
	r.NoError(json.NewDecoder(rc).Decode(&manifest))
	r.NotNil(manifest.Subject, "ownership referrer manifest must carry a subject")
	assert.Equal(t, subject.Digest, manifest.Subject.Digest, "referrer subject must match the target subject manifest digest")
}

// listOwnershipReferrers walks the OCI Referrers API for the subject identified by
// reference — a full OCI reference, by tag or by digest — and returns every referrer
// carrying [annotations.OwnershipArtifactType]. It serves both a by-value subject
// (component-descriptors repo @ local-blob digest) and a by-reference OCI image
// (its access' ImageReference).
func listOwnershipReferrers(t *testing.T, ctx context.Context, resolver *urlresolver.CachingResolver, reference string) []ociImageSpecV1.Descriptor {
	t.Helper()
	r := require.New(t)
	store, err := resolver.StoreForReference(ctx, reference)
	r.NoError(err)
	graphStore, ok := store.(content.ReadOnlyGraphStorage)
	r.Truef(ok, "store %T must implement content.ReadOnlyGraphStorage for referrers discovery", store)
	ref, err := looseref.ParseReference(reference)
	r.NoError(err)
	subject, err := store.Resolve(ctx, ref.ReferenceOrTag())
	r.NoError(err)
	refs, err := orasregistry.Referrers(ctx, graphStore, subject, annotations.OwnershipArtifactType)
	r.NoError(err)
	return refs
}

// newOwnershipRepository builds an oci.Repository backed by resolver with a
// per-test temp dir.
func newOwnershipRepository(t *testing.T, resolver *urlresolver.CachingResolver) *oci.Repository {
	t.Helper()
	repo, err := oci.NewRepository(
		oci.WithResolver(resolver),
		oci.WithTempDir(t.TempDir()),
	)
	require.NoError(t, err)
	return repo
}

// ownershipRegistry starts an htpasswd-protected distribution registry and returns
// a resolver pointing at it together with its connection details. The container is
// torn down on test cleanup.
func ownershipRegistry(t *testing.T) (*urlresolver.CachingResolver, *internal.OCIRegistry) {
	t.Helper()
	r := require.New(t)

	reg, err := internal.CreateOCIRegistry(t)
	r.NoError(err)

	resolver, err := urlresolver.New(
		urlresolver.WithBaseURL(reg.RegistryAddress),
		urlresolver.WithPlainHTTP(true),
		urlresolver.WithBaseClient(internal.CreateAuthClient(reg.RegistryAddress, reg.User, reg.Password)),
	)
	r.NoError(err)
	return resolver, reg
}
