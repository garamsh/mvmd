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

// probeClone is the helper mode: wait for a byte on stdin, then create a
// sleep into the cgroup named by args[0] and report whether it lives.
func probeClone(args []string) {
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	f, err := os.Open(args[0])
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: int(f.Fd())}
	if err := cmd.Start(); err != nil {
		fmt.Println("start:", err)
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		fmt.Printf("child ENDED (%v)\n", err)
	case <-time.After(500 * time.Millisecond):
		_ = cmd.Process.Kill()
		<-done
		fmt.Println("child alive")
	}
}

func TestProbe_CloneFromAKilledCgroup(t *testing.T) {
	var out strings.Builder
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
	run := func(name string, parent, target string) {
		cmd := exec.Command(os.Args[0], "__probe__", target)
		in, _ := cmd.StdinPipe()
		var buf strings.Builder
		cmd.Stdout = &buf
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(&out, "%s: start %v\n", name, err)
			return
		}
		_ = os.WriteFile(filepath.Join(parent, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		_, _ = in.Write([]byte("x"))
		_ = cmd.Wait()
		fmt.Fprintf(&out, "%s: %s", name, buf.String())
	}
	killed := mk()
	k := exec.Command("/bin/sleep", "30")
	_ = k.Start()
	_ = os.WriteFile(filepath.Join(killed, "cgroup.procs"), []byte(strconv.Itoa(k.Process.Pid)), 0o644)
	_ = os.WriteFile(filepath.Join(killed, "cgroup.kill"), []byte("1"), 0o644)
	_ = k.Wait()
	time.Sleep(100 * time.Millisecond)
	live := mk()
	run("control: parent in a live cgroup, target a fresh cgroup", live, mk())
	run("parent in the killed cgroup, target a fresh cgroup", killed, mk())
	sub := filepath.Join(killed, "sub")
	_ = os.Mkdir(sub, 0o755)
	t.Cleanup(func() { _ = os.Remove(sub) })
	run("parent in the killed cgroup, target a child of it", killed, sub)
	run("parent in a live cgroup, target the killed cgroup", live, killed)
	t.Fatalf("probe result:\n%s", out.String())
}
