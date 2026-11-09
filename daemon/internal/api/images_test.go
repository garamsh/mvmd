package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const pulled = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// puller is an image.Service that answers every Pull with err and keeps the
// reference it was asked for.
type puller struct {
	image.Service
	err error
	got []image.Reference
}

func (p *puller) Pull(_ context.Context, ref image.Reference) error {
	p.got = append(p.got, ref)
	return p.err
}

func TestPullImage_PassesTheReferenceAndMapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		kind   v1alpha1.OutcomeKind
		reason string
	}{
		{"success", nil, v1alpha1.OutcomeKind_OUTCOME_KIND_SUCCESS, ""},
		{"an invalid reference", machine.NewError(machine.ErrInvalid, "no repository"), v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED, "no repository"},
		{"a failed step", machine.NewError(machine.ErrFailed, "fetch the manifest: not found"), v1alpha1.OutcomeKind_OUTCOME_KIND_FAILED, "fetch the manifest: not found"},
		{"an error of no kind", errors.New("context deadline exceeded"), v1alpha1.OutcomeKind_OUTCOME_KIND_FAILED, "context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &puller{err: tt.err}
			s := api.New(nil, nil, p, nil, nil)

			resp, err := s.PullImage(context.Background(), &v1alpha1.PullImageRequest{
				Image: &v1alpha1.ImageRef{Repository: "registry.example/ns/app", Digest: pulled},
			})

			assert.Equal(t, []image.Reference{{Repository: "registry.example/ns/app", Digest: pulled}}, p.got, "the use case is asked for what the request names")
			if tt.err == nil {
				require.NoError(t, err)
				assert.NotNil(t, resp)
				return
			}
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Equal(t, tt.kind, v1alpha1.KindOf(err))
			assert.Contains(t, status.Convert(err).Message(), tt.reason, "the reason reaches the caller")
		})
	}
}
