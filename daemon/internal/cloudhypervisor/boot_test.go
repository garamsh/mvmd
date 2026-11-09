package cloudhypervisor_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
)

func TestBoot_ChecksTheVersionThenWaitsForTheVMToBeRunning(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)

	seen := f.seen()
	require.Len(t, seen, 2, "the VM is configured by the command line, so Boot sends no configuration")
	assert.Equal(t, request{"GET", "/vmm.ping", ""}, seen[0])
	assert.Equal(t, request{"GET", "/vm.info", ""}, seen[1])
}

func TestBoot_WaitsWhileTheVMIsNotCreatedOrNotYetRunning(t *testing.T) {
	n := newNode(t)
	var polls atomic.Int32
	f := &fakeAPI{answer: func(r request) (int, string) {
		switch r.Path {
		case "/vmm.ping":
			return http.StatusOK, `{"version":"v54.0"}`
		case "/vm.info":
			switch polls.Add(1) {
			case 1:
				return http.StatusNotFound, "VM instance is not created"
			case 2:
				return http.StatusOK, `{"state":"Created"}`
			}
			return http.StatusOK, `{"state":"Running"}`
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())

	require.NoError(t, n.driver.Boot(context.Background(), "m1", n.spec()))
	assert.Equal(t, int32(3), polls.Load(), "not created, created, running")
}

func TestBoot_FailsForAVMThatIsNeitherCreatedNorRunning(t *testing.T) {
	for _, state := range []string{"Shutdown", "Paused", "BreakPoint"} {
		t.Run(state, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{answer: func(r request) (int, string) {
				if r.Path == "/vm.info" {
					return http.StatusOK, `{"state":"` + state + `"}`
				}
				return http.StatusOK, `{"version":"v54.0"}`
			}}
			f.listen(t, n.apiSocket())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := n.driver.Boot(ctx, "m1", n.spec())
			require.Error(t, err)
			assert.Contains(t, err.Error(), state)
			assert.NotErrorIs(t, err, context.DeadlineExceeded, "it did not wait out the context")
		})
	}
}

func TestBoot_ReportsAnyOtherRefusalOfVMInfo(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		if r.Path == "/vm.info" {
			return http.StatusInternalServerError, "the VMM is broken"
		}
		return http.StatusOK, `{"version":"v54.0"}`
	}}
	f.listen(t, n.apiSocket())
	err := n.driver.Boot(context.Background(), "m1", n.spec())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the VMM is broken")
}

func TestBoot_AcceptsTheVersionWithOrWithoutItsPrefixAndRefusesAnother(t *testing.T) {
	for _, tt := range []struct {
		version string
		ok      bool
	}{{"v54.0", true}, {"54.0", true}, {"54.0.0", true}, {"v54.0.1", true}, {"54.01", false}, {"v54.10.0", false}, {"v53.0", false}, {"v54.1", false}, {"", false}} {
		t.Run(tt.version, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{answer: func(r request) (int, string) {
				if r.Path == "/vmm.ping" {
					return http.StatusOK, `{"version":"` + tt.version + `"}`
				}
				return http.StatusOK, `{"state":"Running"}`
			}}
			f.listen(t, n.apiSocket())
			err := n.driver.Boot(context.Background(), "m1", n.spec())
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not 54.0")
			require.Len(t, f.seen(), 1, "only the version was asked")
		})
	}
}

func TestBoot_WaitsForTheSocketTheVMMCreates(t *testing.T) {
	n := newNode(t)
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
		case r.Path == "/vmm.ping":
			return http.StatusOK, `{"version":"v54.0"}`
		case r.Path == "/vm.info" && failing.Load():
			return http.StatusOK, `{"state":"Shutdown"}`
		}
		return http.StatusOK, `{"state":"Running"}`
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.Error(t, n.driver.Boot(ctx, "m1", n.spec()))

	// The failed Boot left nothing behind, and a Driver of a restarted daemon
	// holds nothing of any Describe.
	failing.Store(false)
	restarted, err := cloudhypervisor.New(n.cfg)
	require.NoError(t, err)
	require.NoError(t, restarted.Boot(context.Background(), "m1", n.spec()))
	assert.Equal(t, "/vm.info", f.seen()[len(f.seen())-1].Path)
	assert.NoFileExists(t, filepath.Join(n.run, "boot.json"))
}

func TestBoot_GivesUpWhenTheContextEndsBeforeTheSocketExists(t *testing.T) {
	n := newNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := n.driver.Boot(ctx, "m1", n.spec())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestBoot_GivesUpWhenTheContextEndsBeforeTheVMIsCreated(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		if r.Path == "/vm.info" {
			return http.StatusNotFound, "not created"
		}
		return http.StatusOK, `{"version":"v54.0"}`
	}}
	f.listen(t, n.apiSocket())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := n.driver.Boot(ctx, "m1", n.spec())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestBoot_StopsWaitingOnAnAnswerWhenTheContextEnds(t *testing.T) {
	n := newNode(t)
	release := make(chan struct{})
	f := &fakeAPI{answer: func(r request) (int, string) {
		if r.Path == "/vmm.ping" {
			<-release
		}
		return http.StatusOK, `{"version":"v54.0"}`
	}}
	f.listen(t, n.apiSocket())
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := n.driver.Boot(ctx, "m1", n.spec())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestPauseAndResume_PutTheVMToPausedAndBack(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	require.NoError(t, n.driver.Pause(context.Background(), "m1"))
	require.NoError(t, n.driver.Resume(context.Background(), "m1"))

	seen := f.seen()[before:]
	assert.Equal(t, []request{{"PUT", "/vm.pause", ""}, {"PUT", "/vm.resume", ""}}, seen)
}

func TestPause_ReportsCloudHypervisorsRefusal(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.mu.Lock()
	f.answer = func(request) (int, string) {
		return http.StatusMethodNotAllowed, "the VM is not running"
	}
	f.mu.Unlock()
	err := n.driver.Pause(context.Background(), "m1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the VM is not running")
	assert.Contains(t, err.Error(), "405")
}

func TestPause_NeedsTheVMMToBeThere(t *testing.T) {
	n := newNode(t)
	require.Error(t, n.driver.Pause(context.Background(), "m1"), "no API socket exists")
	require.Error(t, n.driver.Resume(context.Background(), "m1"), "no API socket exists")
}

func TestShutdown_PressesThePowerButtonAndReturnsOnceTheVMMHasEnded(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	afterTheButton(f, f.stop)

	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 5*time.Second))
	last := f.seen()[len(f.seen())-1]
	assert.Equal(t, request{"PUT", "/vm.power-button", ""}, last)
}

func TestShutdown_ReportsAGuestThatDidNotStopWithinTheGrace(t *testing.T) {
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
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())
	start := time.Now()
	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 0))
	assert.Less(t, time.Since(start), time.Second)

	assert.Equal(t, []request{{"PUT", "/vm.power-button", ""}}, f.seen()[before:], "it asked, once")
}

func TestShutdown_ReportsARefusedRequest(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.mu.Lock()
	f.answer = func(request) (int, string) { return http.StatusConflict, "no ACPI" }
	f.mu.Unlock()
	err := n.driver.Shutdown(context.Background(), "m1", time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ACPI")
}

// afterTheButton runs end once the power button has been pressed.
func afterTheButton(f *fakeAPI, end func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = func(r request) (int, string) {
		if r.Path == "/vm.power-button" {
			go func() { time.Sleep(100 * time.Millisecond); end() }()
		}
		return http.StatusNoContent, ""
	}
}

func TestShutdown_ASocketLeftWithNothingListeningMeansTheVMMHasEnded(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	afterTheButton(f, f.stopLeavingTheSocket)
	require.NoError(t, n.driver.Shutdown(context.Background(), "m1", 5*time.Second), "ECONNREFUSED")
}

func TestShutdown_AnyOtherFailureToReachTheSocketIsReportedNotTakenForAnEnd(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	// The socket file is replaced, in one rename, by a link to itself, so the
	// path fails with ELOOP. Removing it first would leave a moment with no
	// file, which reads as an ended VMM.
	afterTheButton(f, func() {
		tmp := filepath.Join(n.run, "loop")
		_ = os.Symlink("api.sock", tmp)
		_ = os.Rename(tmp, n.apiSocket())
	})
	err := n.driver.Shutdown(context.Background(), "m1", 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ELOOP)
	assert.NotContains(t, err.Error(), "did not stop")
}
