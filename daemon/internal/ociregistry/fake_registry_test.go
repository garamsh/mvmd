package ociregistry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/ociregistry"
)

const repoName = "library/app"

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// authMode is how the fake registry asks for credentials.
type authMode int

const (
	anonymous authMode = iota
	basicAuth
	bearerAuth
)

// served is a manifest or index the fake holds under its digest.
type served struct {
	mediaType string
	body      []byte
	// header is the Docker-Content-Digest the registry states, when it states one.
	header string
}

// fakeRegistry is an in-process OCI distribution registry over TLS. Everything
// it holds is under repoName and is found by digest.
type fakeRegistry struct {
	srv *httptest.Server

	mode     authMode
	user     string
	password string

	mu        sync.Mutex
	manifests map[string]served
	blobs     map[string]http.HandlerFunc
	requests  []string
	tokens    int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{manifests: map[string]served{}, blobs: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// withAuth makes the registry refuse requests that do not carry the identity.
func (f *fakeRegistry) withAuth(mode authMode, user, password string) {
	f.mode, f.user, f.password = mode, user, password
}

func (f *fakeRegistry) host() string { return f.srv.Listener.Addr().String() }

func (f *fakeRegistry) repository() string { return f.host() + "/" + repoName }

func (f *fakeRegistry) reference(digest string) image.Reference {
	return image.Reference{Repository: f.repository(), Digest: machine.ImageDigest(digest)}
}

func (f *fakeRegistry) registry(creds func(string) ociregistry.Credential) *ociregistry.Registry {
	opts := ociregistry.Options{HTTPClient: f.srv.Client()}
	if creds != nil {
		opts.Credentials = func(_ context.Context, registry string) (ociregistry.Credential, error) {
			return creds(registry), nil
		}
	}
	return ociregistry.New(opts)
}

// seen returns the paths the registry was asked for.
func (f *fakeRegistry) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeRegistry) tokensIssued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

// addManifest serves body as a manifest of the media type under the digest of
// body, and returns the digest.
func (f *fakeRegistry) addManifest(mediaType string, body []byte) string {
	d := sha(body)
	f.serveManifest(d, mediaType, "", body)
	return d
}

// serveManifest serves body under any digest, as a registry that answers with
// other content than it advertised does.
func (f *fakeRegistry) serveManifest(digest, mediaType, header string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifests[digest] = served{mediaType: mediaType, body: body, header: header}
}

// addBlob serves content under its digest with its Content-Length.
func (f *fakeRegistry) addBlob(content []byte) image.Descriptor {
	d := sha(content)
	f.serveBlob(d, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content)
	})
	return image.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: d, Size: int64(len(content))}
}

// serveBlob answers a request for the blob with the handler.
func (f *fakeRegistry) serveBlob(digest string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[digest] = h
}

// serveChunked serves content with no Content-Length, so the registry's own
// framing does not state where the blob ends.
func (f *fakeRegistry) serveChunked(digest string, content []byte) {
	f.serveBlob(digest, func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		w.Write(content)
	})
}

func (f *fakeRegistry) challenge(w http.ResponseWriter) {
	switch f.mode {
	case basicAuth:
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	case bearerAuth:
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/token",service="fake"`, f.host()))
	}
	w.WriteHeader(http.StatusUnauthorized)
}

// authorized reports whether the request carries what the registry asks for.
func (f *fakeRegistry) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	switch f.mode {
	case basicAuth:
		u, p, ok := r.BasicAuth()
		return ok && u == f.user && p == f.password
	case bearerAuth:
		return h == "Bearer token-for-"+f.user
	}
	return true
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	if r.URL.Path == "/token" {
		u, p, ok := r.BasicAuth()
		if f.user != "" && (!ok || u != f.user || p != f.password) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"token": "token-for-" + f.user})
		return
	}
	if !f.authorized(r) {
		f.challenge(w)
		return
	}
	prefix := "/v2/" + repoName
	switch {
	case r.URL.Path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, prefix+"/manifests/"):
		f.mu.Lock()
		m, ok := f.manifests[strings.TrimPrefix(r.URL.Path, prefix+"/manifests/")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", m.mediaType)
		if m.header != "" {
			w.Header().Set("Docker-Content-Digest", m.header)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(m.body)))
		w.Write(m.body)
	case strings.HasPrefix(r.URL.Path, prefix+"/blobs/"):
		f.mu.Lock()
		h, ok := f.blobs[strings.TrimPrefix(r.URL.Path, prefix+"/blobs/")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	default:
		http.NotFound(w, r)
	}
}

// imageOf publishes a config and layers and returns the manifest naming them,
// published under its digest, with the descriptors.
func (f *fakeRegistry) imageOf(config string, layers ...[]byte) (digest string, manifest ocispec.Manifest) {
	cfg := []byte(config)
	cd := f.addBlob(cfg)
	manifest = ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    toSpec(cd, ocispec.MediaTypeImageConfig),
	}
	for _, l := range layers {
		manifest.Layers = append(manifest.Layers, toSpec(f.addBlob(l), "application/vnd.oci.image.layer.v1.tar"))
	}
	manifest.SchemaVersion = 2
	body, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return f.addManifest(ocispec.MediaTypeImageManifest, body), manifest
}

func toSpec(d image.Descriptor, mediaType string) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.Digest(d.Digest), Size: d.Size}
}
