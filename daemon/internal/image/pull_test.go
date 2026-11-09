package image_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestPull_PublishesTheConvertedImage(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")

	require.NoError(t, r.pull(ref))

	r.requireListed(t, ref)
	located, err := r.store.Locate(context.Background(), ref.Digest)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecSpec{
		Command:    []string{"/bin/app", "--serve", "one"},
		Env:        map[string]string{"A": "1", "B": "x=y"},
		WorkingDir: "/srv",
	}, located.Run, "the entrypoint and command are one argument vector")

	record, ok := r.store.RecordOf(ref.Digest)
	require.True(t, ok)
	assert.Equal(t, located.UUID, record.UUID)
	assert.Equal(t, "converter-1", record.Converter)
	assert.Equal(t, "amd64", record.Architecture)
	assert.NotEmpty(t, record.Verity, "the digest Seal measured reaches the record")
	assert.Zero(t, r.store.Staging(), "the staging area is removed after the publish")
	assert.Zero(t, r.uids.Held())
}

func TestPull_HandsTheConverterTheVerifiedLayersInOrder(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")

	require.NoError(t, r.pull(ref))

	jobs := r.conv.Jobs()
	require.Len(t, jobs, 1)
	job := jobs[0]
	hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
	assert.Equal(t, hex[0:8]+"-"+hex[8:12]+"-"+hex[12:16]+"-"+hex[16:20]+"-"+hex[20:32], job.FilesystemUUID)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`), job.FilesystemUUID)
	assert.Equal(t, r.settings.NameSetBytes, job.NameSetBytes)
	require.Len(t, job.Layers, 2)
	assert.Equal(t, []string{layerTar, layerGzip}, []string{job.Layers[0].MediaType, job.Layers[1].MediaType})
}

func TestPull_RepeatOfAPresentImageChangesNothing(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	require.NoError(t, r.pull(ref))
	steps := []string{"stage", "manifest", "config", "layer", "put-layer", "describe", "start", "boot", "convert", "terminate", "seal", "record", "publish", "discard"}
	before := map[string]int{}
	for _, s := range steps {
		before[s] = r.calls(s)
		require.NotZero(t, before[s], "step %s ran in the first pull", s)
	}
	record, _ := r.store.RecordOf(ref.Digest)

	require.NoError(t, r.pull(ref))

	for _, s := range steps {
		assert.Equal(t, before[s], r.calls(s), "step %s ran again", s)
	}
	after, _ := r.store.RecordOf(ref.Digest)
	assert.Equal(t, record, after)
	assert.Equal(t, []machine.ImageDigest{ref.Digest}, r.store.Listed())

	// The control: another digest does run the steps the repeat skipped.
	require.NoError(t, r.pull(r.add("two")))
	assert.Greater(t, r.calls("convert"), before["convert"])
}

func TestPull_RejectsAReferenceThatNamesNoDigest(t *testing.T) {
	valid := strings.Repeat("a", 64)
	tests := []struct {
		name string
		ref  image.Reference
	}{
		{"a tag", image.Reference{Repository: "registry.example/app", Digest: "latest"}},
		{"no digest", image.Reference{Repository: "registry.example/app"}},
		{"another algorithm", image.Reference{Repository: "registry.example/app", Digest: machine.ImageDigest("sha512:" + valid)}},
		{"upper case hex", image.Reference{Repository: "registry.example/app", Digest: machine.ImageDigest("sha256:" + strings.ToUpper(valid))}},
		{"short hex", image.Reference{Repository: "registry.example/app", Digest: machine.ImageDigest("sha256:" + valid[:63])}},
		{"trailing text", image.Reference{Repository: "registry.example/app", Digest: machine.ImageDigest("sha256:" + valid + "x")}},
		{"no repository", image.Reference{Digest: machine.ImageDigest("sha256:" + valid)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			requireKind(t, r.pull(tt.ref), machine.ErrInvalid)
			assert.Zero(t, r.calls("locate"), "an invalid reference reached the cache")
			assert.Zero(t, r.calls("stage"))
			assert.Zero(t, r.calls("manifest"))
		})
	}

	t.Run("the control, a digest, is accepted", func(t *testing.T) {
		r := newRig(t)
		ref := r.add("one")
		require.NoError(t, r.pull(ref))
		assert.NotZero(t, r.calls("locate"))
	})
}

func TestPull_RefusesLayersTheConverterDoesNotTake(t *testing.T) {
	r := newRig(t)
	bad := r.registry.Add("registry.example/app", memory.Image{
		Platform: platform,
		Layers: []memory.Layer{
			{MediaType: layerTar, Content: []byte("fine")},
			{MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", Content: []byte("not an OCI type")},
		},
	})

	err := requireKind(t, r.pull(bad), machine.ErrFailed)

	assert.Contains(t, err.Reason, "not tar, tar+gzip or tar+zstd")
	assert.Zero(t, r.calls("layer"), "a layer was read before the type was refused")
	assert.Zero(t, r.calls("convert"))
	r.requireNothingVisible(t, bad)

	// The control: the same shape with taken types converts.
	good := r.add("good")
	require.NoError(t, r.pull(good))
	r.requireListed(t, good)
}

func TestPull_RefusesARunConfigurationItCannotRead(t *testing.T) {
	r := newRig(t)
	bad := r.registry.Add("registry.example/app", memory.Image{
		Platform: platform,
		Config:   image.Config{Entrypoint: []string{"/bin/app"}, Env: []string{"NO_EQUALS_SIGN"}},
		Layers:   []memory.Layer{{MediaType: layerTar, Content: []byte("layer")}},
	})

	err := requireKind(t, r.pull(bad), machine.ErrFailed)

	assert.Contains(t, err.Reason, "NO_EQUALS_SIGN")
	assert.Zero(t, r.calls("layer"), "layers were fetched for an image that cannot run")
	r.requireNothingVisible(t, bad)

	good := r.add("good")
	require.NoError(t, r.pull(good))
}

func TestPull_RefusesALayerTheRegistryServedWrong(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	manifest, err := r.registry.Manifest(context.Background(), ref, platform)
	require.NoError(t, err)
	r.registry.Tamper(manifest.Layers[1], []byte("TOP LAYER OF ONE"))

	err = r.pull(ref)

	requireKind(t, err, machine.ErrFailed)
	assert.Contains(t, err.Error(), "digest")
	assert.Zero(t, r.calls("convert"), "the converter was given unverified layers")
	r.requireNothingVisible(t, ref)

	// The control: the same layer served as advertised is pulled.
	r.registry.Tamper(manifest.Layers[1], []byte("top layer of one"))
	require.NoError(t, r.pull(ref))
	r.requireListed(t, ref)
}

func TestPull_RefusesALengthTheConverterCannotHaveWritten(t *testing.T) {
	tests := []struct {
		name   string
		length uint64
	}{
		{"zero", 0},
		{"past the output file", outputMax + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			ref := r.add("one")
			r.conv.Report(tt.length)

			err := requireKind(t, r.pull(ref), machine.ErrFailed)

			assert.Contains(t, err.Reason, "reported a length")
			assert.Zero(t, r.calls("seal"), "the output was truncated to an untrusted length")
			r.requireNothingVisible(t, ref)

			// The control: the largest length the output holds is accepted.
			r.conv.Report(outputMax)
			require.NoError(t, r.pull(ref))
		})
	}
}
