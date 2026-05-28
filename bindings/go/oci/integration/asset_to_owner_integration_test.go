package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/opencontainers/go-digest"
	ociImageSpecV1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/modules/registry"
	"oras.land/oras-go/v2/content"
	orasregistry "oras.land/oras-go/v2/registry"

	"ocm.software/open-component-model/bindings/go/blob/inmemory"
	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	"ocm.software/open-component-model/bindings/go/oci"
	urlresolver "ocm.software/open-component-model/bindings/go/oci/resolver/url"
	"ocm.software/open-component-model/bindings/go/oci/spec/annotations"
	"ocm.software/open-component-model/bindings/go/oci/spec/layout"
	ocmruntime "ocm.software/open-component-model/bindings/go/runtime"
)

// Test_Integration_AssetToOwner verifies the asset-to-owner scenario
// end-to-end (ADR 0016): a by-value OCI resource uploaded through the OCM
// OCI binding must be discoverable as an ownership referrer via the OCI
// Distribution Referrers API.
//
// Verification goes through the ORAS Go SDK (`registry.Referrers`,
// `store.Fetch`) against a live containerised registry — the same API path
// that every OCI v1.1 client uses under the covers.
func Test_Integration_AssetToOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	password := generateRandomPassword(t, passwordLength)
	htpasswd := generateHtpasswd(t, testUsername, password)

	t.Logf("Launching test registry (%s)...", distributionRegistryImage)
	registryContainer, err := registry.Run(ctx, distributionRegistryImage,
		registry.WithHtpasswd(htpasswd),
		testcontainers.WithEnv(map[string]string{
			"REGISTRY_VALIDATION_DISABLED": "true",
			"REGISTRY_LOG_LEVEL":           "debug",
		}),
		testcontainers.WithLogger(log.TestLogger(t)),
	)
	r := require.New(t)
	r.NoError(err)
	t.Cleanup(func() {
		r.NoError(testcontainers.TerminateContainer(registryContainer))
	})

	registryAddress, err := registryContainer.HostAddress(ctx)
	r.NoError(err)

	resolver, err := urlresolver.New(
		urlresolver.WithBaseURL(registryAddress),
		urlresolver.WithPlainHTTP(true),
		urlresolver.WithBaseClient(createAuthClient(registryAddress, testUsername, password)),
	)
	r.NoError(err)

	repo, err := oci.NewRepository(
		oci.WithResolver(resolver),
		oci.WithTempDir(t.TempDir()),
	)
	r.NoError(err)

	const (
		componentName    = "ocm.software/asset-to-owner-test"
		componentVersion = "v1.0.0"
		resourceName     = "backend-image"
	)

	t.Run("create component version and verify single ownership referrer", func(t *testing.T) {
		r := require.New(t)
		resourceDigest := uploadResource(t, ctx, repo, componentName, componentVersion, resourceName, []byte("ownership-payload"))

		referrers := listOwnershipReferrers(t, ctx, resolver, componentName, componentVersion, resourceDigest)
		r.Len(referrers, 1, "exactly one ownership referrer should be discoverable via the Referrers API")
		ref := referrers[0]

		t.Run("software.ocm.component.name and .version", func(t *testing.T) {
			assert.Equal(t, componentName, ref.Annotations[annotations.OwnershipComponentName])
			assert.Equal(t, componentVersion, ref.Annotations[annotations.OwnershipComponentVersion])
		})

		t.Run("software.ocm.artifact (identity and kind)", func(t *testing.T) {
			var payload struct {
				Identity map[string]string `json:"identity"`
				Kind     string            `json:"kind"`
			}
			require.NoError(t, json.Unmarshal([]byte(ref.Annotations[annotations.ArtifactAnnotationKey]), &payload))
			assert.Equal(t, "resource", payload.Kind)
			assert.Equal(t, resourceName, payload.Identity["name"])
			assert.Equal(t, componentVersion, payload.Identity["version"])
		})
	})

	t.Run("multiple resources in a CV each get their own referrer", func(t *testing.T) {
		const (
			multiComponent = "ocm.software/asset-to-owner-multi-asset"
			backendName    = "backend-image"
			frontendName   = "frontend-image"
		)
		r := require.New(t)
		backendDigest := uploadResource(t, ctx, repo, multiComponent, componentVersion, backendName, []byte("backend-payload"))
		frontendDigest := uploadResource(t, ctx, repo, multiComponent, componentVersion, frontendName, []byte("frontend-payload"))
		r.NotEqual(backendDigest, frontendDigest, "distinct payloads must produce distinct subject digests")

		cases := []struct {
			label   string
			subject digest.Digest
			want    string
		}{
			{"backend", backendDigest, backendName},
			{"frontend", frontendDigest, frontendName},
		}
		for _, tc := range cases {
			t.Run(tc.label, func(t *testing.T) {
				referrers := listOwnershipReferrers(t, ctx, resolver, multiComponent, componentVersion, tc.subject)
				require.Len(t, referrers, 1, "exactly one referrer per asset")

				var payload struct {
					Identity map[string]string `json:"identity"`
					Kind     string            `json:"kind"`
				}
				require.NoError(t, json.Unmarshal([]byte(referrers[0].Annotations[annotations.ArtifactAnnotationKey]), &payload))
				assert.Equal(t, tc.want, payload.Identity["name"],
					"%s referrer must point at its own asset, not the sibling", tc.label)
			})
		}
	})

	t.Run("re-uploading the same resource leaves a single referrer", func(t *testing.T) {
		// The referrer manifest omits org.opencontainers.image.created, so every
		// re-upload produces an identical manifest digest and the registry returns
		// the existing one instead of indexing a new referrer. End-to-end proof
		// of `ocm add cv` idempotency at the referrer layer.
		var resourceDigest digest.Digest
		for i := range 3 {
			resourceDigest = uploadResource(t, ctx, repo, componentName, componentVersion, resourceName, []byte("ownership-payload"))
			require.NotEmptyf(t, resourceDigest, "re-upload attempt %d must yield a digest", i+1)
		}

		referrers := listOwnershipReferrers(t, ctx, resolver, componentName, componentVersion, resourceDigest)
		assert.Lenf(t, referrers, 1,
			"identical re-uploads must converge on a single referrer; got %d distinct manifests", len(referrers))
	})

	t.Run("external relation: resource uploads without an ownership referrer", func(t *testing.T) {
		// Locks in the opt-out contract: a referrer is created only for resources
		// owned by the component (local relation). An external resource must be
		// accepted by value and leave the Referrers API empty for that subject.
		const (
			externalComponent = "ocm.software/asset-to-owner-test-external"
			externalResource  = "backend-image-external"
		)
		resourceDigest := uploadResourceWithRelation(t, ctx, repo, externalComponent, componentVersion, externalResource, []byte("ownership-payload-external"), descriptor.ExternalRelation)

		referrers := listOwnershipReferrers(t, ctx, resolver, externalComponent, componentVersion, resourceDigest)
		assert.Emptyf(t, referrers,
			"external relation must not push any ownership referrer; found %d", len(referrers))
	})
}

// uploadResource pushes a one-layer OCI image as a local resource (local
// relation) through repo and returns the digest of the resulting subject
// manifest.
func uploadResource(t *testing.T, ctx context.Context, repo *oci.Repository, component, version, name string, payload []byte) digest.Digest {
	t.Helper()
	return uploadResourceWithRelation(t, ctx, repo, component, version, name, payload, descriptor.LocalRelation)
}

// uploadResourceWithRelation pushes a one-layer OCI image as a local resource
// with the given relation through repo and returns the digest of the resulting
// subject manifest.
func uploadResourceWithRelation(t *testing.T, ctx context.Context, repo *oci.Repository, component, version, name string, payload []byte, relation descriptor.ResourceRelation) digest.Digest {
	t.Helper()
	r := require.New(t)
	data, _ := createSingleLayerOCIImage(t, payload)
	res := &descriptor.Resource{
		ElementMeta: descriptor.ElementMeta{
			ObjectMeta: descriptor.ObjectMeta{Name: name, Version: version},
		},
		Type:     "ociArtifact",
		Relation: relation,
		Access: &v2.LocalBlob{
			Type: ocmruntime.Type{
				Name:    v2.LocalBlobAccessType,
				Version: v2.LocalBlobAccessTypeVersion,
			},
			MediaType:      layout.MediaTypeOCIImageLayoutTarGzipV1,
			LocalReference: digest.FromBytes(data).String(),
		},
	}
	newRes, err := repo.AddLocalResource(ctx, component, version, res, inmemory.New(bytes.NewReader(data)))
	r.NoError(err)
	var localAccess v2.LocalBlob
	r.NoError(v2.Scheme.Convert(newRes.Access, &localAccess))
	return digest.Digest(localAccess.LocalReference)
}

// listOwnershipReferrers walks the OCI Referrers API for subjectDigest and
// returns every referrer carrying [annotations.OwnershipArtifactType].
func listOwnershipReferrers(t *testing.T, ctx context.Context, resolver *urlresolver.CachingResolver, component, version string, subjectDigest digest.Digest) []ociImageSpecV1.Descriptor {
	t.Helper()
	r := require.New(t)
	compRef := resolver.ComponentVersionReference(ctx, component, version)
	store, err := resolver.StoreForReference(ctx, compRef)
	r.NoError(err)
	graphStore, ok := store.(content.ReadOnlyGraphStorage)
	r.Truef(ok, "store %T must implement content.ReadOnlyGraphStorage for referrers discovery", store)
	subject, err := store.Resolve(ctx, subjectDigest.String())
	r.NoError(err)
	refs, err := orasregistry.Referrers(ctx, graphStore, subject, annotations.OwnershipArtifactType)
	r.NoError(err)
	return refs
}

// Test_Integration_TransferOwnershipReferrer verifies the transfer half of
// ADR 0016 end-to-end: an ownership referrer attached to a by-value resource on
// a source registry is present on the target registry after transfer.
//
// The flow mirrors `ocm transfer` of an OCI artifact stored by value:
// GetLocalResource pulls the resource (and, via the live Referrers API, its
// ownership referrer) into an OCI layout, then AddLocalResource on the target
// copies that layout — referrer included — into the target repository.
//
// Two registries are used so both sides can carry the SAME component name, as
// `ocm transfer` does (it preserves the component name; only the repository
// changes).
func Test_Integration_TransferOwnershipReferrer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := require.New(t)

	const (
		component    = "ocm.software/transfer-ownership-test"
		version      = "v1.0.0"
		resourceName = "backend-image"
	)

	// The resource has local relation, so both source and target create an
	// ownership referrer on upload, and the source's referrer also travels in the
	// transferred layout and is copied across. All three converge on the same
	// digest, so the target ends up with exactly one referrer.
	srcResolver := launchOwnershipRegistry(t, ctx)
	dstResolver := launchOwnershipRegistry(t, ctx)

	srcRepo, err := oci.NewRepository(
		oci.WithResolver(srcResolver),
		oci.WithTempDir(t.TempDir()),
	)
	r.NoError(err)

	dstRepo, err := oci.NewRepository(
		oci.WithResolver(dstResolver),
		oci.WithTempDir(t.TempDir()),
	)
	r.NoError(err)

	// 1. Author the resource on the source. Because the resource has local
	//    relation the upload also pushes one ownership referrer, and a component
	//    version is added so the resource is resolvable via GetLocalResource.
	data, _ := createSingleLayerOCIImage(t, []byte("transfer-payload"))
	resource := &descriptor.Resource{
		Relation:    descriptor.LocalRelation,
		ElementMeta: descriptor.ElementMeta{ObjectMeta: descriptor.ObjectMeta{Name: resourceName, Version: version}},
		Type:        "ociArtifact",
		Access: &v2.LocalBlob{
			Type:           ocmruntime.Type{Name: v2.LocalBlobAccessType, Version: v2.LocalBlobAccessTypeVersion},
			MediaType:      layout.MediaTypeOCIImageLayoutTarGzipV1,
			LocalReference: digest.FromBytes(data).String(),
		},
	}
	srcRes, err := srcRepo.AddLocalResource(ctx, component, version, resource, inmemory.New(bytes.NewReader(data)))
	r.NoError(err)

	srcDesc := &descriptor.Descriptor{
		Meta: descriptor.Meta{Version: "v2"},
		Component: descriptor.Component{
			Provider:      descriptor.Provider{Name: "ocm.software"},
			ComponentMeta: descriptor.ComponentMeta{ObjectMeta: descriptor.ObjectMeta{Name: component, Version: version}},
			Resources:     []descriptor.Resource{*srcRes},
		},
	}
	r.NoError(srcRepo.AddComponentVersion(ctx, srcDesc))

	// Sanity: the source must carry exactly the created ownership referrer.
	var srcAccess v2.LocalBlob
	r.NoError(v2.Scheme.Convert(srcRes.Access, &srcAccess))
	srcReferrers := listOwnershipReferrers(t, ctx, srcResolver, component, version, digest.Digest(srcAccess.LocalReference))
	r.Len(srcReferrers, 1, "the source must carry the created ownership referrer")

	// 2. Transfer: GetLocalResource pulls the resource and its ownership referrer
	//    into a layout; AddLocalResource on the target copies it across.
	blobContent, transferRes, err := srcRepo.GetLocalResource(ctx, component, version, ocmruntime.Identity{
		"name":    resourceName,
		"version": version,
	})
	r.NoError(err)

	// GetLocalResource re-materializes the layout, so its bytes — and thus the
	// local-blob digest — differ from the source's; clear the carried digest so
	// the target adopts the re-packed blob's digest. This is orthogonal to the
	// referrer copy under test (which is independent of the layout digest).
	transferRes.Digest = nil

	dstRes, err := dstRepo.AddLocalResource(ctx, component, version, transferRes, blobContent)
	r.NoError(err)

	// 3. The ownership referrer must be discoverable on the TARGET via the live
	//    Referrers API — and there must be exactly one despite creation and the
	//    copy both running, because they converge on the same digest.
	var dstAccess v2.LocalBlob
	r.NoError(v2.Scheme.Convert(dstRes.Access, &dstAccess))
	dstReferrers := listOwnershipReferrers(t, ctx, dstResolver, component, version, digest.Digest(dstAccess.LocalReference))
	r.Len(dstReferrers, 1, "the ownership referrer must be copied to the target registry")

	assert.Equal(t, component, dstReferrers[0].Annotations[annotations.OwnershipComponentName],
		"the copied referrer must retain its component name")
	assert.Equal(t, version, dstReferrers[0].Annotations[annotations.OwnershipComponentVersion],
		"the copied referrer must retain its component version")
}

// launchOwnershipRegistry starts an htpasswd-protected distribution registry and
// returns a resolver pointing at it. The container is torn down on test cleanup.
func launchOwnershipRegistry(t *testing.T, ctx context.Context) *urlresolver.CachingResolver {
	t.Helper()
	r := require.New(t)

	password := generateRandomPassword(t, passwordLength)
	htpasswd := generateHtpasswd(t, testUsername, password)

	registryContainer, err := registry.Run(ctx, distributionRegistryImage,
		registry.WithHtpasswd(htpasswd),
		testcontainers.WithEnv(map[string]string{
			"REGISTRY_VALIDATION_DISABLED": "true",
		}),
		testcontainers.WithLogger(log.TestLogger(t)),
	)
	r.NoError(err)
	t.Cleanup(func() {
		r.NoError(testcontainers.TerminateContainer(registryContainer))
	})

	address, err := registryContainer.HostAddress(ctx)
	r.NoError(err)

	resolver, err := urlresolver.New(
		urlresolver.WithBaseURL(address),
		urlresolver.WithPlainHTTP(true),
		urlresolver.WithBaseClient(createAuthClient(address, testUsername, password)),
	)
	r.NoError(err)
	return resolver
}
