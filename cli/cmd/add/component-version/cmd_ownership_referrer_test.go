package componentversion

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ocm.software/open-component-model/bindings/go/blob"
	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
)

// plainResourcePlugin implements resource.Repository without the optional
// ownership-referrer capability.
type plainResourcePlugin struct{}

func (plainResourcePlugin) GetResourceCredentialConsumerIdentity(context.Context, *descriptor.Resource) (runtime.Identity, error) {
	return nil, nil
}

func (plainResourcePlugin) UploadResource(context.Context, *descriptor.Resource, blob.ReadOnlyBlob, runtime.Typed) (*descriptor.Resource, error) {
	return nil, nil
}

func (plainResourcePlugin) DownloadResource(context.Context, *descriptor.Resource, runtime.Typed) (blob.ReadOnlyBlob, error) {
	return nil, nil
}

// attachingResourcePlugin additionally implements the AddOwnershipReferrer
// capability and records the forwarded call.
type attachingResourcePlugin struct {
	plainResourcePlugin
	called bool
	err    error
}

func (p *attachingResourcePlugin) AddOwnershipReferrer(ctx context.Context, component, version string, res *descriptor.Resource, credentials runtime.Typed) error {
	p.called = true
	return p.err
}

func TestConstructorPlugin_AddOwnershipReferrer(t *testing.T) {
	ctx := context.Background()
	res := &descriptor.Resource{}

	t.Run("plugin without the capability is a no-op", func(t *testing.T) {
		c := &constructorPlugin{plugin: plainResourcePlugin{}}
		require.NoError(t, c.AddOwnershipReferrer(ctx, "comp", "v1", res, nil))
	})

	t.Run("capable plugin is forwarded and its error propagates", func(t *testing.T) {
		p := &attachingResourcePlugin{err: fmt.Errorf("push denied")}
		c := &constructorPlugin{plugin: p}
		err := c.AddOwnershipReferrer(ctx, "comp", "v1", res, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "push denied")
		assert.True(t, p.called, "the capable plugin must be invoked")
	})
}
