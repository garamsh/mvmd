package ociregistry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"sync"
)

// rootsTransport sends each request over a transport that verifies against the
// roots read for that request, and builds a new transport when they changed.
type rootsTransport struct {
	roots func(ctx context.Context) (*x509.CertPool, error)

	mu   sync.Mutex
	pool *x509.CertPool
	next *http.Transport
}

// RoundTrip implements http.RoundTripper.
func (t *rootsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pool, err := t.roots(req.Context())
	if err != nil {
		return nil, err
	}
	return t.transport(pool).RoundTrip(req)
}

func (t *rootsTransport) transport(pool *x509.CertPool) *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.next == nil || !t.pool.Equal(pool) {
		if t.next != nil {
			t.next.CloseIdleConnections()
		}
		next := http.DefaultTransport.(*http.Transport).Clone()
		next.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		t.pool, t.next = pool, next
	}
	return t.next
}
