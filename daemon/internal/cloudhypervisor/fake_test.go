package cloudhypervisor_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// request is one request the fake API socket received.
type request struct {
	Method, Path, Body string // Path is under /api/v1
}

// fakeAPI stands in for Cloud Hypervisor's API socket. It records every request and
// answers from a handler: 204 for anything unless the test says otherwise.
type fakeAPI struct {
	mu       sync.Mutex
	requests []request
	// answer, when set, decides the status and body of a request.
	answer func(r request) (int, string)
	srv    *http.Server
	ln     *net.UnixListener
}

// listen makes the socket exist at path.
func (f *fakeAPI) listen(t *testing.T, path string) {
	t.Helper()
	ln := listenUnix(t, path)
	f.ln = ln
	f.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path, ok := strings.CutPrefix(r.URL.Path, "/api/v1")
		req := request{r.Method, path, string(body)}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		answer := f.answer
		f.mu.Unlock()
		status, out := http.StatusNoContent, ""
		switch {
		case !ok:
			status, out = http.StatusNotFound, "not under /api/v1"
		case req.Method == "GET" && req.Path == "/vmm.ping":
			status, out = http.StatusOK, `{"version":"v54.0","build_version":"v54.0","pid":7}`
		case req.Method == "GET" && req.Path == "/vm.info":
			status, out = http.StatusOK, `{"config":{},"state":"Running"}`
		}
		if answer != nil {
			status, out = answer(req)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	})}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
}

// stop ends the VMM: its socket no longer accepts connections, and the file
// is gone.
func (f *fakeAPI) stop() { _ = f.srv.Close() }

// stopLeavingTheSocket ends the VMM as a killed one does: the socket file stays
// and nothing listens on it.
func (f *fakeAPI) stopLeavingTheSocket() {
	f.ln.SetUnlinkOnClose(false)
	_ = f.srv.Close()
}

func (f *fakeAPI) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

// node is a Driver over files in a temporary directory, with the run
// directory of every Machine the one it names. The directory's path is over
// what a Unix socket address holds, so that every test reaches a socket as a
// Machine with a long state directory does.
type node struct {
	t      *testing.T
	run    string
	cfg    cloudhypervisor.Config
	driver *cloudhypervisor.Driver
}

type runDirs struct{ dir string }

func (r runDirs) RunDir(machine.MachineID) (string, error) { return r.dir, nil }

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newNode(t *testing.T) *node {
	t.Helper()
	root := longTempDir(t, "ch")
	write := func(name, content string) string {
		p := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}
	run := filepath.Join(root, "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	require.Greater(t, len(filepath.Join(root, "run", "vsock.sock")), 107, "the control: a socket in the run directory is over what an address holds")
	cfg := cloudhypervisor.Config{
		Binary: write("cloud-hypervisor", "a binary"), BinarySHA256: sum([]byte("a binary")),
		Kernel: write("vmlinux", "a kernel"), KernelSHA256: sum([]byte("a kernel")),
		Initrd: write("initrd.cpio", "an initrd"), InitrdSHA256: sum([]byte("an initrd")),
		Dirs: runDirs{run},
	}
	d, err := cloudhypervisor.New(cfg)
	require.NoError(t, err)
	return &node{t: t, run: run, cfg: cfg, driver: d}
}

// listenUnix binds a socket at path the way the Driver reaches it, through a
// descriptor of its directory, so that a path over the 107 bytes an address
// holds works. The descriptor is held until the test ends.
func listenUnix(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	fd, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	l, err := net.Listen("unix", fmt.Sprintf("/proc/self/fd/%d/%s", fd, filepath.Base(path)))
	require.NoError(t, err)
	return l.(*net.UnixListener)
}

// longTempDir makes a directory whose path is over the 107 bytes a Unix socket
// address holds, so that a socket in it is reachable only through a descriptor
// of the directory.
func longTempDir(t *testing.T, prefix string) string {
	t.Helper()
	root, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, strings.Repeat("d", 100))
	require.NoError(t, os.Mkdir(dir, 0o755))
	return dir
}

func (n *node) apiSocket() string   { return filepath.Join(n.run, "api.sock") }
func (n *node) vsockSocket() string { return filepath.Join(n.run, "vsock.sock") }

// spec is a Machine's boot description: its image, its root disk and a tap.
func (n *node) spec() machine.BootSpec {
	return machine.BootSpec{
		Devices: []machine.BlockDevice{
			{Path: "/images/erofs", ReadOnly: true},
			{Disk: "root", Path: "/disks/root.img"},
		},
		Network:   machine.NetworkDevice{Tap: "tap-m1", MAC: "02:00:00:00:00:01"},
		Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20},
		RunDir:    n.run,
	}
}

// booted describes and boots a Machine against a fake API socket.
func (n *node) booted(f *fakeAPI) {
	n.t.Helper()
	f.listen(n.t, n.apiSocket())
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(n.t, err)
	require.NoError(n.t, n.driver.Boot(context.Background(), "m1", n.spec()))
}
