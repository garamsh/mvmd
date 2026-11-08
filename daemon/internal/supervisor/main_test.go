package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Landlock and seccomp apply to the process that installs them for good, so
// each test runs its case in a child: this test binary again, with the case
// named in the environment. Neither needs privilege, only no_new_privs.

const (
	caseEnv = "MVMD_SUPERVISOR_TEST_CASE"
	argsEnv = "MVMD_SUPERVISOR_TEST_ARGS"
)

// cases are the functions a child runs, each returning what it observed.
var cases = map[string]func(args []byte) (map[string]string, error){}

func TestMain(m *testing.M) {
	if name := os.Getenv(caseEnv); name != "" {
		os.Exit(runCase(name))
	}
	os.Exit(m.Run())
}

func runCase(name string) int {
	// Landlock and seccomp bind the thread that installs them, so the case
	// stays on one.
	runtime.LockOSThread()
	if name == "sleep" {
		time.Sleep(time.Minute)
		return 0
	}
	fn, ok := cases[name]
	if !ok {
		os.Stderr.WriteString("no such case: " + name + "\n")
		return 2
	}
	res, err := fn([]byte(os.Getenv(argsEnv)))
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
	return 0
}

// child runs the case in a child of this test binary and returns what it
// reported, or the exit error and its stderr.
func child(t *testing.T, name string, args any) (map[string]string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), caseEnv+"="+name, argsEnv+"="+string(raw))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Join(err, errors.New(stderr.String()))
	}
	var res map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("the child reported %q: %v", stdout.String(), err)
	}
	return res, nil
}

// errName names the errno inside err, or says ok.
func errName(err error) string {
	if err == nil {
		return "ok"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if n := unix.ErrnoName(errno); n != "" {
			return n
		}
	}
	return err.Error()
}

// noNewPrivs is what the confinement sets before Landlock and seccomp.
func noNewPrivs() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}
