package ociregistry_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/ociregistry"
)

var amd64 = image.Platform{OS: "linux", Architecture: "amd64"}

func TestRegistry_ConfigWithoutAUserHasNone(t *testing.T) {
	f := newFakeRegistry(t)
	digest, _ := f.imageOf(`{"config":{"Cmd":["-v"]}}`, []byte("layer"))
	r := f.registry(nil)
	ref := f.reference(digest)
	m, err := r.Manifest(context.Background(), ref, amd64)
	require.NoError(t, err)

	c, err := r.Config(context.Background(), ref, m.Config)
	require.NoError(t, err)
	assert.Equal(t, image.Config{Cmd: []string{"-v"}}, c)
}

func TestRegistry_ImageIsFetchedByDigest(t *testing.T) {
	f := newFakeRegistry(t)
	layerA, layerB := []byte("layer a"), []byte("layer b, longer")
	digest, _ := f.imageOf(`{"config":{"Entrypoint":["/app"],"Cmd":["-v"],"Env":["A=1"],"WorkingDir":"/srv","User":"app:grp"}}`, layerA, layerB)
	r := f.registry(nil)
	ref := f.reference(digest)

	m, err := r.Manifest(context.Background(), ref, amd64)
	require.NoError(t, err)
	require.Len(t, m.Layers, 2)
	assert.Equal(t, sha(layerA), m.Layers[0].Digest)
	assert.Equal(t, int64(len(layerB)), m.Layers[1].Size)

	c, err := r.Config(context.Background(), ref, m.Config)
	require.NoError(t, err)
	assert.Equal(t, image.Config{Entrypoint: []string{"/app"}, Cmd: []string{"-v"}, Env: []string{"A=1"}, WorkingDir: "/srv", User: "app:grp"}, c)

	rc, err := r.Layer(context.Background(), ref, m.Layers[1])
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, layerB, got)

	for _, path := range f.seen() {
		assert.NotContains(t, path, "latest", "no request resolved a tag")
		if strings.Contains(path, "/manifests/") || strings.Contains(path, "/blobs/") {
			assert.Contains(t, path, "/sha256:", "every fetch named a digest")
		}
	}
}

func TestRegistry_ARefusedReferenceReachesNoRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	good, _ := f.imageOf(`{}`, []byte("x"))
	r := f.registry(nil)

	_, err := r.Manifest(context.Background(), f.reference(good), amd64)
	require.NoError(t, err, "control: the digest is fetched")
	before := len(f.seen())

	for name, ref := range map[string]image.Reference{
		"a tag in the repository":      {Repository: f.repository() + ":latest", Digest: f.reference(good).Digest},
		"a digest in the repository":   {Repository: f.repository() + "@" + good, Digest: f.reference(good).Digest},
		"a tag as the digest":          {Repository: f.repository(), Digest: "latest"},
		"a digest of another function": {Repository: f.repository(), Digest: machine.ImageDigest("sha512:" + strings.Repeat("a", 128))},
		"a short digest":               {Repository: f.repository(), Digest: "sha256:abcd"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.Manifest(context.Background(), ref, amd64)
			assert.Error(t, err)
			_, err = r.Layer(context.Background(), ref, image.Descriptor{Digest: good, Size: 1})
			assert.Error(t, err)
		})
	}
	assert.Len(t, f.seen(), before, "no request left for a refused reference")
}

func TestRegistry_IndexSelectsThePlatformManifest(t *testing.T) {
	f := newFakeRegistry(t)
	amdDigest, _ := f.imageOf(`{}`, []byte("amd layer"))
	armDigest, arm := f.imageOf(`{"config":{"Cmd":["arm"]}}`, []byte("arm layer"))
	index := func(entries ...ocispec.Descriptor) string {
		body, err := json.Marshal(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: entries})
		require.NoError(t, err)
		return f.addManifest(ocispec.MediaTypeImageIndex, body)
	}
	entry := func(digest string, size int, arch string) ocispec.Descriptor {
		return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digestOfString(digest), Size: int64(size),
			Platform: &ocispec.Platform{OS: "linux", Architecture: arch}}
	}
	size := func(digest string) int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.manifests[digest].body)
	}
	both := index(entry(amdDigest, size(amdDigest), "amd64"), entry(armDigest, size(armDigest), "arm64"))
	onlyAmd := index(entry(amdDigest, size(amdDigest), "amd64"))
	r := f.registry(nil)

	got, err := r.Manifest(context.Background(), f.reference(both), image.Platform{OS: "linux", Architecture: "arm64"})
	require.NoError(t, err)
	assert.Equal(t, arm.Layers[0].Digest.String(), got.Layers[0].Digest, "the arm64 manifest, not the first one")

	_, err = r.Manifest(context.Background(), f.reference(onlyAmd), amd64)
	require.NoError(t, err, "control: the same index serves the platform it has")
	_, err = r.Manifest(context.Background(), f.reference(onlyAmd), image.Platform{OS: "linux", Architecture: "arm64"})
	require.ErrorContains(t, err, "no manifest for linux/arm64")
}

func digestOfString(s string) digest.Digest { return digest.Digest(s) }

func TestRegistry_ManifestFromAnotherContentIsRefused(t *testing.T) {
	f := newFakeRegistry(t)
	good, _ := f.imageOf(`{}`, []byte("x"))
	r := f.registry(nil)
	_, err := r.Manifest(context.Background(), f.reference(good), amd64)
	require.NoError(t, err, "control: the bytes the digest names are accepted")

	other, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest})
	asked := sha([]byte("the manifest the pull asked for"))
	f.serveManifest(asked, ocispec.MediaTypeImageManifest, asked, other)
	_, err = r.Manifest(context.Background(), f.reference(asked), amd64)
	require.ErrorIs(t, err, ociregistry.ErrDigest, "a registry stating the digest asked for over other bytes")

	f.serveManifest(asked, ocispec.MediaTypeImageManifest, "", other)
	_, err = r.Manifest(context.Background(), f.reference(asked), amd64)
	require.ErrorContains(t, err, "digest mismatch", "a registry stating nothing, which the client library checks")
}

func TestRegistry_ChosenManifestMustBeWhatTheIndexDescribes(t *testing.T) {
	f := newFakeRegistry(t)
	child, _ := f.imageOf(`{}`, []byte("x"))
	f.mu.Lock()
	childBody := f.manifests[child].body
	f.mu.Unlock()
	build := func(size int) string {
		body, err := json.Marshal(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(child), Size: int64(size),
			Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}}}})
		require.NoError(t, err)
		return f.addManifest(ocispec.MediaTypeImageIndex, body)
	}
	r := f.registry(nil)

	_, err := r.Manifest(context.Background(), f.reference(build(len(childBody))), amd64)
	require.NoError(t, err, "control: the index states the manifest's size")
	_, err = r.Manifest(context.Background(), f.reference(build(len(childBody)-1)), amd64)
	require.Error(t, err, "an index stating another size than the manifest has")

	other := bytes.Clone(childBody)
	other[len(other)-1] ^= 1
	f.serveManifest(child, ocispec.MediaTypeImageManifest, child, other)
	_, err = r.Manifest(context.Background(), f.reference(build(len(childBody))), amd64)
	require.ErrorIs(t, err, ociregistry.ErrDigest, "other bytes of the size the index states, under the digest it names")
}

func TestRegistry_ManifestOfAnotherKindIsRefused(t *testing.T) {
	f := newFakeRegistry(t)
	body := []byte(`{"schemaVersion":2}`)
	oci := f.addManifest(ocispec.MediaTypeImageManifest, body)
	docker := f.addManifest("application/vnd.docker.distribution.manifest.v2+json", append(body, ' '))
	r := f.registry(nil)

	_, err := r.Manifest(context.Background(), f.reference(oci), amd64)
	assert.ErrorContains(t, err, "manifest config", "control: an OCI manifest is read, and this one is refused only for what it holds")
	_, err = r.Manifest(context.Background(), f.reference(docker), amd64)
	assert.ErrorContains(t, err, "not an OCI image manifest or index")
}

// readLayer returns what a pull of the layer reads, and the error that ended
// it.
func readLayer(t *testing.T, f *fakeRegistry, layer image.Descriptor) ([]byte, error) {
	t.Helper()
	rc, err := f.registry(nil).Layer(context.Background(), f.reference(sha([]byte("ref"))), layer)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestLayer_ExcessIsRefusedAtTheFirstByteBeyondTheSize(t *testing.T) {
	f := newFakeRegistry(t)
	content := []byte("0123456789")
	layer := f.addBlob(content)

	f.serveChunked(layer.Digest, content)
	got, err := readLayer(t, f, layer)
	require.NoError(t, err, "control: the same stream of exactly its size is accepted")
	assert.Equal(t, content, got)

	f.serveChunked(layer.Digest, append(bytes.Clone(content), 'X'))
	got, err = readLayer(t, f, layer)
	require.ErrorIs(t, err, ociregistry.ErrSize)
	assert.Equal(t, content, got, "every byte handed on is within the size, and the excess byte is not")

	f.serveChunked(layer.Digest, append(bytes.Clone(content), bytes.Repeat([]byte{'X'}, 1<<20)...))
	got, err = readLayer(t, f, layer)
	require.ErrorIs(t, err, ociregistry.ErrSize)
	assert.Len(t, got, len(content))
}

func TestLayer_ExcessIsRefusedOneByteAtATime(t *testing.T) {
	f := newFakeRegistry(t)
	content := []byte("0123456789")
	layer := f.addBlob(content)
	f.serveChunked(layer.Digest, append(bytes.Clone(content), 'X'))

	rc, err := f.registry(nil).Layer(context.Background(), f.reference(sha([]byte("ref"))), layer)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(iotest.OneByteReader(rc))
	require.ErrorIs(t, err, ociregistry.ErrSize)
	assert.Equal(t, content, got)
}

func TestLayer_AContentLengthOtherThanTheSizeIsRefusedBeforeAnyByte(t *testing.T) {
	f := newFakeRegistry(t)
	content := []byte("0123456789")
	layer := f.addBlob(content)
	served := false
	f.serveBlob(layer.Digest, func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.Header().Set("Content-Length", fmt.Sprint(len(content)+1))
		w.Write(append(bytes.Clone(content), 'X'))
	})

	_, err := readLayer(t, f, layer)
	require.Error(t, err)
	assert.True(t, served, "the registry was asked")
}

func TestLayer_ShortBlobFailsAtItsEnd(t *testing.T) {
	f := newFakeRegistry(t)
	content := []byte("0123456789")
	layer := f.addBlob(content)

	f.serveChunked(layer.Digest, content)
	_, err := readLayer(t, f, layer)
	require.NoError(t, err, "control: the full stream is accepted")

	f.serveChunked(layer.Digest, content[:9])
	_, err = readLayer(t, f, layer)
	require.ErrorIs(t, err, ociregistry.ErrSize)
	assert.NotErrorIs(t, err, ociregistry.ErrDigest)
}

func TestLayer_WrongBytesOfTheRightSizeFailAtTheEnd(t *testing.T) {
	f := newFakeRegistry(t)
	content := []byte("0123456789")
	layer := f.addBlob(content)
	got, err := readLayer(t, f, layer)
	require.NoError(t, err, "control: the bytes the digest names are accepted")
	require.Equal(t, content, got)

	wrong := []byte("0123456780")
	f.serveBlob(layer.Digest, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(wrong)))
		w.Write(wrong)
	})
	_, err = readLayer(t, f, layer)
	require.ErrorIs(t, err, ociregistry.ErrDigest)
	assert.NotErrorIs(t, err, ociregistry.ErrSize)
}

func TestLayer_ABlobTheRegistryLacksFails(t *testing.T) {
	f := newFakeRegistry(t)
	_, err := readLayer(t, f, image.Descriptor{MediaType: "x", Digest: sha([]byte("absent")), Size: 6})
	require.ErrorContains(t, err, "fetch the layer")
}

func TestConfig_IsVerifiedAndBounded(t *testing.T) {
	f := newFakeRegistry(t)
	digest, m := f.imageOf(`{"config":{"Cmd":["x"]}}`)
	ref := f.reference(digest)
	r := f.registry(nil)
	cfg := image.Descriptor{MediaType: m.Config.MediaType, Digest: m.Config.Digest.String(), Size: m.Config.Size}

	_, err := r.Config(context.Background(), ref, cfg)
	require.NoError(t, err, "control")

	wrong := bytes.Repeat([]byte{'x'}, int(cfg.Size))
	f.serveBlob(cfg.Digest, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(wrong)))
		w.Write(wrong)
	})
	_, err = r.Config(context.Background(), ref, cfg)
	require.ErrorIs(t, err, ociregistry.ErrDigest)

	huge := f.addBlob(make([]byte, 5<<20))
	_, err = r.Config(context.Background(), ref, huge)
	require.ErrorIs(t, err, ociregistry.ErrSize, "a config over the bound is not read")
}

func TestRegistry_Authentication(t *testing.T) {
	creds := func(user, password string) func(string) ociregistry.Credential {
		return func(string) ociregistry.Credential { return ociregistry.Credential{Username: user, Password: password} }
	}
	pull := func(f *fakeRegistry, digest string, c func(string) ociregistry.Credential) error {
		_, err := f.registry(c).Manifest(context.Background(), f.reference(digest), amd64)
		return err
	}

	t.Run("anonymous", func(t *testing.T) {
		f := newFakeRegistry(t)
		d, _ := f.imageOf(`{}`, []byte("x"))
		require.NoError(t, pull(f, d, nil))
	})
	t.Run("basic", func(t *testing.T) {
		f := newFakeRegistry(t)
		d, _ := f.imageOf(`{}`, []byte("x"))
		f.withAuth(basicAuth, "node", "s3cret")
		require.Error(t, pull(f, d, nil), "control: without credentials the registry refuses")
		require.Error(t, pull(f, d, creds("node", "wrong")))
		require.NoError(t, pull(f, d, creds("node", "s3cret")))
	})
	t.Run("token", func(t *testing.T) {
		f := newFakeRegistry(t)
		d, _ := f.imageOf(`{}`, []byte("x"))
		f.withAuth(bearerAuth, "node", "s3cret")
		require.Error(t, pull(f, d, nil), "control: without credentials the token service refuses")
		err := pull(f, d, creds("node", "wrong"))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "wrong", "a refused credential is not in the error")
		assert.Zero(t, f.tokensIssued())
		require.NoError(t, pull(f, d, creds("node", "s3cret")))
		assert.Equal(t, 1, f.tokensIssued())
	})
	t.Run("anonymous token", func(t *testing.T) {
		f := newFakeRegistry(t)
		d, _ := f.imageOf(`{}`, []byte("x"))
		f.withAuth(bearerAuth, "", "")
		require.NoError(t, pull(f, d, nil))
		assert.Equal(t, 1, f.tokensIssued())
	})
	t.Run("credentials are asked for by registry host", func(t *testing.T) {
		f := newFakeRegistry(t)
		d, _ := f.imageOf(`{}`, []byte("x"))
		f.withAuth(basicAuth, "node", "s3cret")
		var asked []string
		c := func(registry string) ociregistry.Credential {
			asked = append(asked, registry)
			return ociregistry.Credential{Username: "node", Password: "s3cret"}
		}
		require.NoError(t, pull(f, d, c))
		assert.Contains(t, asked, f.host())
	})
}

func TestCredential_IsNeverPrinted(t *testing.T) {
	c := ociregistry.Credential{Username: "node", Password: "s3cret"}

	for _, form := range []string{"%v", "%+v", "%s"} {
		assert.NotContains(t, fmt.Sprintf(form, c), "s3cret", form)
	}
	var out bytes.Buffer
	slog.New(slog.NewTextHandler(&out, nil)).Info("pull", "credential", c)
	assert.NotContains(t, out.String(), "s3cret")
	assert.Contains(t, out.String(), "pull", "control: the line was written")
}

// stalledLayer serves 64 bytes, the first 32 at once and the rest when release
// is closed. gone is closed when the registry sees the request's connection
// end first.
func stalledLayer(f *fakeRegistry) (layer image.Descriptor, content []byte, release, gone chan struct{}) {
	content = bytes.Repeat([]byte{'a'}, 64)
	layer = f.addBlob(content)
	release, gone = make(chan struct{}), make(chan struct{})
	f.serveBlob(layer.Digest, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content[:32])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(gone)
		case <-release:
			w.Write(content[32:])
		}
	})
	return layer, content, release, gone
}

func TestLayer_AStalledTransferCompletesWhenTheRegistryResumes(t *testing.T) {
	f := newFakeRegistry(t)
	layer, content, release, _ := stalledLayer(f)
	rc, err := f.registry(nil).Layer(context.Background(), f.reference(sha([]byte("ref"))), layer)
	require.NoError(t, err)
	defer rc.Close()
	close(release)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestLayer_CancellationStopsATransfer(t *testing.T) {
	f := newFakeRegistry(t)
	layer, content, _, gone := stalledLayer(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := f.registry(nil).Layer(ctx, f.reference(sha([]byte("ref"))), layer)
	require.NoError(t, err)
	defer rc.Close()

	first := make([]byte, 32)
	_, err = io.ReadFull(rc, first)
	require.NoError(t, err)
	require.Equal(t, content[:32], first)
	cancel()
	ended := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(rc)
		ended <- err
	}()
	select {
	case err = <-ended:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not end after the context was cancelled")
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the registry never saw the transfer end")
	}
}

func TestManifest_CancelledContextStopsTheCall(t *testing.T) {
	f := newFakeRegistry(t)
	d, _ := f.imageOf(`{}`, []byte("x"))
	r := f.registry(nil)
	_, err := r.Manifest(context.Background(), f.reference(d), amd64)
	require.NoError(t, err, "control")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Manifest(ctx, f.reference(d), amd64)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRegistry_RootsAreReadAtEachRequest(t *testing.T) {
	f := newFakeRegistry(t)
	d, _ := f.imageOf(`{}`, []byte("x"))
	trusted := x509.NewCertPool()
	trusted.AddCert(f.srv.Certificate())
	roots := x509.NewCertPool()
	r := ociregistry.New(ociregistry.Options{RootCAs: func(context.Context) (*x509.CertPool, error) { return roots, nil }})

	_, err := r.Manifest(context.Background(), f.reference(d), amd64)
	require.Error(t, err, "control: a registry whose certificate no root vouches for is refused")
	assert.Contains(t, err.Error(), "certificate")

	roots = trusted
	_, err = r.Manifest(context.Background(), f.reference(d), amd64)
	require.NoError(t, err, "the same Registry trusts the root once it is returned")
}
