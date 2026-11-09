package firecracker_test

import (
	"context"
	"net/http"
	"runtime"
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
		{"PUT", "/boot-source", `{"kernel_image_path":"` + n.cfg.Kernel + `","initrd_path":"` + n.cfg.Initrd + `","boot_args":"` + firecracker.BootArgs(runtime.GOARCH) + `"}`},
		{"PUT", "/drives/disk0", `{"drive_id":"disk0","path_on_host":"/images/erofs","is_root_device":false,"is_read_only":true}`},
		{"PUT", "/drives/disk1", `{"drive_id":"disk1","path_on_host":"/disks/root.img","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}`},
		{"PUT", "/network-interfaces/eth0", `{"iface_id":"eth0","host_dev_name":"tap-m1","guest_mac":"02:00:00:00:00:01"}`},
		{"PUT", "/vsock", `{"guest_cid":3,"uds_path":"` + n.vsockSocket() + `"}`},
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

func TestBoot_AMachineWithNoNetworkDeviceGetsNone(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	spec := n.spec()
	spec.Network = machine.NetworkDevice{}
	spec.Devices = spec.Devices[1:]
	_, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	require.NoError(t, n.driver.Boot(context.Background(), "m1"))

	var paths []string
	for _, r := range f.seen() {
		paths = append(paths, r.Path)
	}
	assert.Equal(t, []string{"/version", "/machine-config", "/boot-source", "/drives/disk0", "/vsock", "/actions"}, paths)
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

	err = n.driver.Boot(context.Background(), "m1")
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

	err = n.driver.Boot(context.Background(), "m1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot open the kernel")
	assert.Contains(t, err.Error(), "/boot-source")
	assert.Len(t, f.seen(), 3, "version, machine-config, boot-source; nothing after the refusal")
}

func TestBoot_OfAMachineNotDescribedFails(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	require.Error(t, n.driver.Boot(context.Background(), "m1"))
	assert.Empty(t, f.seen())
}

func TestBoot_WaitsForTheSocketTheVMMCreates(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	f := &fakeAPI{}
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.listen(t, n.apiSocket())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, n.driver.Boot(ctx, "m1"))
	assert.NotEmpty(t, f.seen())
}

func TestBoot_GivesUpWhenTheContextEndsBeforeTheSocketExists(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = n.driver.Boot(ctx, "m1")
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
	err = n.driver.Boot(ctx, "m1")
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
	start := time.Now()
	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 0))
	assert.Less(t, time.Since(start), time.Second)
}
