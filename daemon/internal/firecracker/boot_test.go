package firecracker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// onX86 skips a test of what Firecracker v1.17.0 does on x86_64 only.
func onX86(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("Ctrl-Alt-Del is sent on x86_64 only")
	}
}

func TestBoot_ConfiguresTheVMMThenStartsTheGuest(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)

	seen := f.seen()
	want := []struct{ method, path, body string }{
		{"GET", "/version", ""},
		{"PUT", "/machine-config", `{"vcpu_count":2,"mem_size_mib":256}`},
		{"PUT", "/boot-source", `{"kernel_image_path":"/boot/kernel","initrd_path":"/boot/initrd","boot_args":"` + firecracker.BootArgs(runtime.GOARCH) + `"}`},
		{"PUT", "/drives/disk0", `{"drive_id":"disk0","path_on_host":"/disk/0","is_root_device":false,"is_read_only":true}`},
		{"PUT", "/drives/disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2", `{"drive_id":"disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2","path_on_host":"/disk/1","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}`},
		{"PUT", "/network-interfaces/eth0", `{"iface_id":"eth0","host_dev_name":"tap-m1","guest_mac":"02:00:00:00:00:01"}`},
		{"PUT", "/vsock", `{"guest_cid":3,"uds_path":"/run/vsock.sock"}`},
		{"PUT", "/actions", `{"action_type":"InstanceStart"}`},
	}
	require.Len(t, seen, len(want))
	for i, w := range want {
		assert.Equal(t, w.method, seen[i].Method, "request %d", i)
		assert.Equal(t, w.path, seen[i].Path, "request %d", i)
		if w.body != "" {
			assert.JSONEq(t, w.body, seen[i].Body, "request %d", i)
		}
	}
}

// A snapshot records the paths of the devices the VMM was configured with, and
// a restored Machine has its own files at other host paths (ADR 0019), so what
// reaches the VMM names the fixed paths in its root and no host path.
func TestBoot_NamesOnlyPathsInTheVMMsRoot(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	spec := n.spec()
	p, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)

	named := map[string]string{} // request path and field to the path the VMM is given
	for _, r := range f.seen() {
		var body map[string]any
		if r.Body == "" {
			continue
		}
		require.NoError(t, json.Unmarshal([]byte(r.Body), &body))
		for _, field := range []string{"kernel_image_path", "initrd_path", "path_on_host", "uds_path"} {
			if v, ok := body[field]; ok {
				named[r.Path+" "+field] = v.(string)
			}
		}
	}
	assert.Equal(t, map[string]string{
		"/boot-source kernel_image_path": "/boot/kernel",
		"/boot-source initrd_path":       "/boot/initrd",
		"/drives/disk0 path_on_host":     "/disk/0",
		"/drives/disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2 path_on_host": "/disk/1",
		"/vsock uds_path": "/run/vsock.sock",
	}, named, "control: every request that carries a path was read")

	// Every path the VMM is given, in its requests and its arguments, is one
	// the description mounts.
	mounted := map[string]bool{machine.InRunDir: true}
	var hosts []string
	for _, bf := range p.BootFiles {
		mounted[bf.In] = true
		hosts = append(hosts, bf.Host)
	}
	hosts = append(hosts, n.run)
	given := append([]string{}, p.Args[1], p.Args[len(p.Args)-1])
	for _, v := range named {
		given = append(given, v)
	}
	for _, path := range given {
		assert.True(t, mounted[path] || mounted[filepath.Dir(path)], "%s is not a path the description mounts", path)
		for _, host := range hosts {
			assert.NotEqual(t, host, path, "the VMM is given a host path")
		}
	}
}

func TestBoot_AMachineWithNoNetworkDeviceGetsNone(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	spec := n.spec()
	spec.Network = machine.NetworkDevice{}
	spec.Devices = spec.Devices[1:]
	_, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	require.NoError(t, n.driver.Boot(context.Background(), "m1", spec))

	var paths []string
	for _, r := range f.seen() {
		paths = append(paths, r.Path)
	}
	assert.Equal(t, []string{"/version", "/machine-config", "/boot-source", "/drives/disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2", "/vsock", "/actions"}, paths)
}

func TestBoot_RefusesAVMMOfAnotherVersionAndConfiguresNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		if r.Path == "/version" {
			return http.StatusOK, `{"firecracker_version":"1.16.0"}`
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	err = n.driver.Boot(context.Background(), "m1", n.spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1.16.0")
	require.Len(t, f.seen(), 1, "only the version was asked")
	assert.Equal(t, "/version", f.seen()[0].Path)
}

func TestBoot_StopsAtTheFirstRefusalAndReportsItsMessage(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		switch r.Path {
		case "/version":
			return http.StatusOK, `{"firecracker_version":"1.17.0"}`
		case "/boot-source":
			return http.StatusBadRequest, `{"fault_message":"cannot open the kernel"}`
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	err = n.driver.Boot(context.Background(), "m1", n.spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot open the kernel")
	assert.Contains(t, err.Error(), "/boot-source")
	assert.Len(t, f.seen(), 3, "version, machine-config, boot-source; nothing after the refusal")
}

func TestBoot_ConfiguresFromTheDescriptionItIsGivenAndKeepsNothingFromDescribe(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	given := n.spec()
	given.Resources = machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}
	require.NoError(t, n.driver.Boot(context.Background(), "m1", given))

	var config string
	for _, r := range f.seen() {
		if r.Path == "/machine-config" {
			config = r.Body
		}
	}
	assert.JSONEq(t, `{"vcpu_count":4,"mem_size_mib":512}`, config, "what Boot was given, not what Describe was")
	assert.NoFileExists(t, filepath.Join(n.run, "boot.json"), "nothing of the description is left where the VMM's user can write it")
}

func TestBoot_WaitsForTheSocketTheVMMCreates(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	booted := make(chan error, 1)
	go func() { booted <- n.driver.Boot(ctx, "m1", n.spec()) }()

	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-booted:
		t.Fatalf("Boot returned before the socket existed: %v", err)
	default:
	}
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	require.NoError(t, <-booted)
	assert.NotEmpty(t, f.seen())
}

func TestBoot_CanBeRepeatedAfterAFailureAndAfterADaemonRestart(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	var failing atomic.Bool
	failing.Store(true)
	f.mu.Lock()
	f.answer = func(r request) (int, string) {
		switch {
		case r.Path == "/version":
			return http.StatusOK, `{"firecracker_version":"1.17.0"}`
		case r.Path == "/boot-source" && failing.Load():
			return http.StatusBadRequest, `{"fault_message":"not yet"}`
		}
		return http.StatusNoContent, ""
	}
	f.mu.Unlock()
	require.Error(t, n.driver.Boot(context.Background(), "m1", n.spec()))

	// The failed Boot left nothing behind, and a Driver of a restarted daemon
	// holds nothing of any Describe.
	failing.Store(false)
	restarted, err := firecracker.New(n.cfg)
	require.NoError(t, err)
	require.NoError(t, restarted.Boot(context.Background(), "m1", n.spec()))
	assert.Equal(t, "/actions", f.seen()[len(f.seen())-1].Path)
}

func TestBoot_GivesUpWhenTheContextEndsBeforeTheSocketExists(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = n.driver.Boot(ctx, "m1", n.spec())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestBoot_StopsWaitingOnAnAnswerWhenTheContextEnds(t *testing.T) {
	n := newNode(t)
	release := make(chan struct{})
	f := &fakeAPI{answer: func(r request) (int, string) {
		switch r.Path {
		case "/version":
			return http.StatusOK, `{"firecracker_version":"1.17.0"}`
		case "/machine-config":
			<-release
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())
	t.Cleanup(func() { close(release) })
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = n.driver.Boot(ctx, "m1", n.spec())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestPauseAndResume_SetTheVMState(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	require.NoError(t, n.driver.Pause(context.Background(), "m1"))
	require.NoError(t, n.driver.Resume(context.Background(), "m1"))

	seen := f.seen()[before:]
	require.Len(t, seen, 2)
	assert.Equal(t, "PATCH", seen[0].Method)
	assert.Equal(t, "/vm", seen[0].Path)
	assert.JSONEq(t, `{"state":"Paused"}`, seen[0].Body)
	assert.Equal(t, "PATCH", seen[1].Method)
	assert.JSONEq(t, `{"state":"Resumed"}`, seen[1].Body)
}

func TestPause_ReportsFirecrackersRefusal(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.mu.Lock()
	f.answer = func(request) (int, string) {
		return http.StatusBadRequest, `{"fault_message":"the microVM is not started"}`
	}
	f.mu.Unlock()
	err := n.driver.Pause(context.Background(), "m1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the microVM is not started")
}

func TestPause_NeedsTheVMMToBeThere(t *testing.T) {
	n := newNode(t)
	require.Error(t, n.driver.Pause(context.Background(), "m1"), "no API socket exists")
}

func TestShutdown_SendsCtrlAltDelAndReturnsOnceTheVMMHasEnded(t *testing.T) {
	onX86(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.mu.Lock()
	f.answer = func(r request) (int, string) {
		if r.Path == "/actions" {
			go func() { time.Sleep(100 * time.Millisecond); f.stop() }()
		}
		return http.StatusNoContent, ""
	}
	f.mu.Unlock()

	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 5*time.Second))
	last := f.seen()[len(f.seen())-1]
	assert.Equal(t, "/actions", last.Path)
	assert.JSONEq(t, `{"action_type":"SendCtrlAltDel"}`, last.Body)
}

// Firecracker v1.17.0 sends Ctrl-Alt-Del on x86_64 only, so a Shutdown on
// another architecture is refused and reaches no VMM.
func TestShutdown_IsRefusedOffX86_64AndSendsNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	firecracker.SetGOARCH(n.driver, "arm64")
	err := n.driver.Shutdown(context.Background(), "m1", 0)
	require.ErrorIs(t, err, machine.ErrUnsupported)
	assert.Empty(t, f.seen()[before:], "no request reached the API socket")

	firecracker.SetGOARCH(n.driver, "amd64")
	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 0), "control: the same call on x86_64 is sent")
	assert.Len(t, f.seen()[before:], 1)
}

func TestShutdown_ReportsAGuestThatDidNotStopWithinTheGrace(t *testing.T) {
	onX86(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)

	start := time.Now()
	err := n.driver.Shutdown(context.Background(), "m1", 300*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not stop")
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "it waited the grace")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestShutdown_WithNoGraceOnlyAsks(t *testing.T) {
	onX86(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())
	start := time.Now()
	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 0))
	assert.Less(t, time.Since(start), time.Second)

	seen := f.seen()[before:]
	require.Len(t, seen, 1, "it asked, once")
	assert.Equal(t, "PUT", seen[0].Method)
	assert.Equal(t, "/actions", seen[0].Path)
	assert.JSONEq(t, `{"action_type":"SendCtrlAltDel"}`, seen[0].Body)
}

// afterTheAction runs end once Firecracker has been sent an action.
func afterTheAction(f *fakeAPI, end func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = func(r request) (int, string) {
		if r.Path == "/actions" {
			go func() { time.Sleep(100 * time.Millisecond); end() }()
		}
		return http.StatusNoContent, ""
	}
}

func TestShutdown_ASocketLeftWithNothingListeningMeansTheVMMHasEnded(t *testing.T) {
	onX86(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	afterTheAction(f, f.stopLeavingTheSocket)
	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 5*time.Second), "ECONNREFUSED")
}

func TestShutdown_AnyOtherFailureToReachTheSocketIsReportedNotTakenForAnEnd(t *testing.T) {
	onX86(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	// The socket file is replaced, in one rename, by a link to itself, so the
	// path fails with ELOOP. Removing it first would leave a moment with no
	// file, which reads as an ended VMM.
	afterTheAction(f, func() {
		tmp := filepath.Join(n.run, "loop")
		_ = os.Symlink("api.sock", tmp)
		_ = os.Rename(tmp, n.apiSocket())
	})
	err := n.driver.Shutdown(context.Background(), "m1", 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ELOOP)
	assert.NotContains(t, err.Error(), "did not stop")
}
