package app_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// node is a daemon started from the composition site, with a client built on
// lib/runtimeapi connected to its socket.
type node struct {
	app    *app.App
	cfg    app.Config
	client v1alpha1.RuntimeServiceClient
	conn   *grpc.ClientConn
	stop   context.CancelFunc
	done   chan error
}

// shortDir returns a directory whose path leaves room for a socket name.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mvmd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func testConfig(t *testing.T) app.Config {
	t.Helper()
	dir := shortDir(t)
	return app.Config{
		Socket:   app.SocketConfig{Path: filepath.Join(dir, "s"), Mode: "0600"},
		StateDir: filepath.Join(dir, "state"),
		Driver:   app.DriverMemory,
	}
}

// start builds the app from the config through the composition site, serves
// it, and connects a client once it is ready.
func start(t *testing.T, cfg app.Config) *node {
	t.Helper()
	n := serve(t, cfg)
	dial, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	var err error
	n.conn, err = runtimeapi.Dial(dial, cfg.Socket.Path)
	require.NoError(t, err)
	n.client = v1alpha1.NewRuntimeServiceClient(n.conn)
	return n
}

// serve builds and serves the app and waits until it is ready, without
// connecting a client.
func serve(t *testing.T, cfg app.Config) *node {
	t.Helper()
	a, err := app.New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{app: a, cfg: cfg, stop: cancel, done: make(chan error, 1)}
	go func() { n.done <- a.Serve(ctx) }()
	select {
	case <-a.Ready():
	case err := <-n.done:
		t.Fatalf("the daemon ended before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon was not ready")
	}
	t.Cleanup(func() { n.shutdown(t) })
	return n
}

// shutdown stops the daemon and waits for it to close the database.
func (n *node) shutdown(t *testing.T) {
	t.Helper()
	if n.stop == nil {
		return
	}
	if n.conn != nil {
		_ = n.conn.Close()
	}
	n.stop()
	select {
	case err := <-n.done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Error("the daemon did not stop")
	}
	n.stop = nil
}

// raw connects without lib/runtimeapi's client-side validation, to send what
// the client would have refused.
func (n *node) raw(t *testing.T) v1alpha1.RuntimeServiceClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+n.cfg.Socket.Path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return v1alpha1.NewRuntimeServiceClient(conn)
}

// requireKind requires err to end a call in the kind, with a reason.
func requireKind(t *testing.T, err error, kind v1alpha1.OutcomeKind) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, kind, v1alpha1.KindOf(err), "%v", err)
	reason := ""
	for _, d := range statusDetails(err) {
		if f, ok := d.(*v1alpha1.Failure); ok {
			assert.Equal(t, kind, f.GetKind())
			reason = f.GetReason()
		}
	}
	assert.NotEmpty(t, reason, "the Failure detail carries the reason")
}

func writeConfig(t *testing.T, cfg app.Config) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(shortDir(t), "mvmd.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func statusDetails(err error) []any {
	st, _ := status.FromError(err)
	return st.Details()
}
