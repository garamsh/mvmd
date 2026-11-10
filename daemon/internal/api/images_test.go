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
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
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
		kind   v1alpha2.OutcomeKind
		reason string
	}{
		{"success", nil, v1alpha2.OutcomeKind_OUTCOME_KIND_SUCCESS, ""},
		{"an invalid reference", machine.NewError(machine.ErrInvalid, "no repository"), v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "no repository"},
		{"a failed step", machine.NewError(machine.ErrFailed, "fetch the manifest: not found"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, "fetch the manifest: not found"},
		{"an error of no kind", errors.New("context deadline exceeded"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, "context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &puller{err: tt.err}
			s := api.New(nil, nil, p, nil, nil)

			resp, err := s.PullImage(context.Background(), &v1alpha2.PullImageRequest{
				Image: &v1alpha2.ImageRef{Repository: "registry.example/ns/app", Digest: pulled},
			})

			assert.Equal(t, []image.Reference{{Repository: "registry.example/ns/app", Digest: pulled}}, p.got, "the use case is asked for what the request names")
			if tt.err == nil {
				require.NoError(t, err)
				assert.NotNil(t, resp)
				return
			}
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Contains(t, status.Convert(err).Message(), tt.reason, "the reason reaches the caller")
		})
	}
}

// imager is a machine.Service whose image calls answer fixed values and record
// what they were asked.
type imager struct {
	machine.Service
	listed  []machine.ImageDigest
	err     error
	removed []machine.ImageDigest
}

func (i *imager) ListImages(context.Context) ([]machine.ImageDigest, error) {
	return i.listed, i.err
}

func (i *imager) RemoveImage(_ context.Context, digest machine.ImageDigest) error {
	i.removed = append(i.removed, digest)
	return i.err
}

func TestListImages_ReportsEveryImageAndMapsTheUseCaseOutcome(t *testing.T) {
	m := &imager{listed: []machine.ImageDigest{pulled, "sha256:b"}}
	resp, err := api.New(m, nil, nil, nil, nil).ListImages(context.Background(), &v1alpha2.ListImagesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Images, 2)
	assert.Equal(t, pulled, resp.Images[0].Digest)
	assert.Equal(t, "sha256:b", resp.Images[1].Digest)

	failing := &imager{err: machine.NewError(machine.ErrFailed, "list: boom")}
	resp, err = api.New(failing, nil, nil, nil, nil).ListImages(context.Background(), &v1alpha2.ListImagesRequest{})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, v1alpha2.KindOf(err))
}

func TestRemoveImage_PassesTheDigestAndMapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"success", nil, v1alpha2.OutcomeKind_OUTCOME_KIND_SUCCESS},
		{"in use", machine.NewError(machine.ErrPreconditionFailed, "in use"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
		{"store failure", machine.NewError(machine.ErrFailed, "remove: boom"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &imager{err: tt.err}

			_, err := api.New(m, nil, nil, nil, nil).RemoveImage(context.Background(), &v1alpha2.RemoveImageRequest{Digest: pulled})

			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Equal(t, []machine.ImageDigest{pulled}, m.removed)
		})
	}
}

func TestRemoveImage_RejectsAnEmptyDigestBeforeTheUseCase(t *testing.T) {
	m := &imager{}
	server := api.New(m, nil, nil, nil, nil)

	_, err := server.RemoveImage(context.Background(), &v1alpha2.RemoveImageRequest{})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Empty(t, m.removed, "the use case was not asked")

	_, err = server.RemoveImage(context.Background(), &v1alpha2.RemoveImageRequest{Digest: pulled}) // control
	assert.NoError(t, err)
	assert.Equal(t, []machine.ImageDigest{pulled}, m.removed)
}
