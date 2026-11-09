//go:build firecracker_real

package e2e_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	manifestType = "application/vnd.oci.image.manifest.v1+json"
	configType   = "application/vnd.oci.image.config.v1+json"
	gzipLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// registry is the fixture registry the test serves: an OCI distribution endpoint, over TLS, for
// the manifests and blobs of the images the test publishes. It answers by digest only, which is
// all mvmd asks.
type registry struct {
	// host is the address images are named under, such as 127.0.0.1:41233.
	host string
	// caFile is the PEM file of the certificate authority that signed the endpoint's certificate;
	// a daemon started with SSL_CERT_FILE naming it trusts this registry and no other.
	caFile string

	mu    sync.Mutex
	blobs map[string]served
}

type served struct {
	mediaType string
	body      []byte
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// startRegistry serves the fixture registry on the loopback until the test ends. The directory
// holds the certificate authority's file, which the daemon reads.
func startRegistry(t *testing.T, dir string) *registry {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mvmd e2e registry CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	caFile := filepath.Join(dir, "registry-ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644))

	r := &registry{caFile: caFile, blobs: map[string]served{}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	r.host = strings.TrimPrefix(server.URL, "https://")
	return r
}

// serve answers the two routes of the distribution specification mvmd reads by digest, and the
// version check.
func (r *registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "read-only registry", http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	var digest string
	for _, route := range []string{"/manifests/", "/blobs/"} {
		if i := strings.LastIndex(rest, route); i >= 0 {
			digest = rest[i+len(route):]
			break
		}
	}
	r.mu.Lock()
	blob, found := r.blobs[digest]
	r.mu.Unlock()
	if !found {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", blob.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(blob.body)))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write(blob.body)
	}
}

func (r *registry) put(mediaType string, body []byte) (digest string, size int) {
	digest = sha256Digest(body)
	r.mu.Lock()
	r.blobs[digest] = served{mediaType: mediaType, body: body}
	r.mu.Unlock()
	return digest, len(body)
}

// ref names a published image the way a PullImage request does.
type ref struct {
	Repository string
	Digest     string
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

// publish serves an image of the layers, given as uncompressed tar archives lowest first, as one
// OCI image manifest in the repository named by name, with each layer gzip-compressed, and
// returns the reference to pull. The entrypoint is the image's.
func (r *registry) publish(name string, entrypoint []string, layers ...[]byte) ref {
	var manifestLayers []descriptor
	var diffIDs []string
	for _, tarball := range layers {
		diffIDs = append(diffIDs, sha256Digest(tarball))
		digest, size := r.put(gzipLayer, gzipped(tarball))
		manifestLayers = append(manifestLayers, descriptor{MediaType: gzipLayer, Digest: digest, Size: size})
	}
	cfg, err := json.Marshal(map[string]any{
		"architecture": goArch(),
		"os":           "linux",
		"config":       map[string]any{"Entrypoint": entrypoint},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		panic(fmt.Sprintf("marshal a literal: %v", err))
	}
	cfgDigest, cfgSize := r.put(configType, cfg)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     manifestType,
		"config":        descriptor{MediaType: configType, Digest: cfgDigest, Size: cfgSize},
		"layers":        manifestLayers,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal a literal: %v", err))
	}
	digest, _ := r.put(manifestType, manifest)
	return ref{Repository: r.host + "/e2e/" + name, Digest: digest}
}
