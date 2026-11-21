package supervision_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Temporary probe for #745: which way of starting a process into a cgroup
// that had cgroup.kill written is ended by the kernel.
func TestProbe_CgroupKill(t *testing.T) {
	var out strings.Builder
	v, _ := os.ReadFile("/proc/version")
	fmt.Fprintf(&out, "kernel: %s", v)
	n := 0
	mk := func() string {
		n++
		p := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("mvmd-probe-%d-%d", os.Getpid(), n))
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(p) })
		return p
	}
	spawn := func(cg string, flags uintptr, viaFD bool) (*exec.Cmd, error) {
		cmd := exec.Command("/bin/sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, Setpgid: true}
		if viaFD {
			f, err := os.Open(cg)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, int(f.Fd())
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		if !viaFD {
			if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
				return nil, err
			}
		}
		return cmd, nil
	}
	alive := func(cmd *exec.Cmd) string {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return fmt.Sprintf("ENDED (%v)", err)
		case <-time.After(500 * time.Millisecond):
			_ = cmd.Process.Kill()
			<-done
			return "alive"
		}
	}
	empty := func(cg string) {
		for i := 0; i < 500; i++ {
			raw, _ := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
			if len(strings.TrimSpace(string(raw))) == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	kill := func(cg string) {
		_ = os.WriteFile(filepath.Join(cg, "cgroup.kill"), []byte("1"), 0o644)
		empty(cg)
	}
	events := func(cg string) string {
		raw, _ := os.ReadFile(filepath.Join(cg, "cgroup.events"))
		return strings.ReplaceAll(strings.TrimSpace(string(raw)), "\n", ",")
	}

	for _, c := range []struct {
		name  string
		flags uintptr
		viaFD bool
		wait  time.Duration
	}{
		{"fd, no namespace", 0, true, 0},
		{"fd, newpid", syscall.CLONE_NEWPID, true, 0},
		{"fd, newpid, 1s later", syscall.CLONE_NEWPID, true, time.Second},
		{"procs file, no namespace", 0, false, 0},
		{"procs file, newpid", syscall.CLONE_NEWPID, false, 0},
	} {
		cg := mk()
		first, err := spawn(cg, c.flags, c.viaFD)
		if err != nil {
			fmt.Fprintf(&out, "%s: first spawn: %v\n", c.name, err)
			continue
		}
		time.Sleep(100 * time.Millisecond)
		kill(cg)
		_ = first.Wait()
		time.Sleep(c.wait)
		fmt.Fprintf(&out, "%s: events after kill: %s\n", c.name, events(cg))
		second, err := spawn(cg, c.flags, c.viaFD)
		if err != nil {
			fmt.Fprintf(&out, "%s: second spawn: %v\n", c.name, err)
			continue
		}
		fmt.Fprintf(&out, "%s: second in the killed cgroup: %s\n", c.name, alive(second))
		third, err := spawn(mk(), c.flags, c.viaFD)
		if err != nil {
			fmt.Fprintf(&out, "%s: third spawn: %v\n", c.name, err)
			continue
		}
		fmt.Fprintf(&out, "%s: one in a fresh cgroup: %s\n", c.name, alive(third))
	}
	t.Fatalf("probe result:\n%s", out.String())
}
