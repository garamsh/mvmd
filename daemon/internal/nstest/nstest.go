// Package nstest runs a test in a network namespace made for it, as root of
// that namespace, for the tests that change the kernel's network configuration
// and need CAP_NET_ADMIN to do it. Nothing of the host's is touched.
//
// A test wrapped in Run starts again as a child of itself in a fresh namespace,
// by the first of these that works:
//
//   - as root: a child with CLONE_NEWNET;
//   - through passwordless sudo, which the GitHub-hosted runner provides: the
//     same, as root;
//   - in a new user namespace with a range of UIDs mapped from /etc/subuid, by
//     unshare(1), which needs no privilege.
//
// Where none works the test fails, and no test is skipped.
package nstest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

const inNamespaceEnv = "MVMD_TEST_IN_NAMESPACE"

// Run runs body in a fresh network namespace.
func Run(t *testing.T, body func(t *testing.T)) {
	t.Helper()
	if os.Getenv(inNamespaceEnv) == "1" {
		body(t)
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := rerun(self, t.Name())
	if err != nil {
		t.Fatalf("the test needs privilege and a network namespace of its own, and did not pass in one: %v\n%s", err, out)
	}
	t.Logf("passed in a network namespace:\n%s", tail(out, 12))
}

// rerun runs the named test again in a fresh namespace and returns what it
// printed. A child that exits 0 has not passed unless it says the test did: a
// pattern that matches no test also exits 0. The name is matched literally, one
// anchored element for each level of subtest.
func rerun(self, name string) (string, error) {
	levels := strings.Split(name, "/")
	for i, l := range levels {
		levels[i] = "^" + regexp.QuoteMeta(l) + "$"
	}
	args := []string{"-test.run", strings.Join(levels, "/"), "-test.v", "-test.timeout", "5m"}
	var out bytes.Buffer
	if err := reexec(self, args, &out); err != nil {
		return out.String(), err
	}
	if !strings.Contains(out.String(), "--- PASS: "+name+" (") {
		return out.String(), fmt.Errorf("the child exited 0 without reporting that %q passed", name)
	}
	return out.String(), nil
}

func reexec(self string, args []string, out io.Writer) error {
	run := func(cmd *exec.Cmd) error {
		cmd.Stdout, cmd.Stderr = out, out
		return cmd.Run()
	}
	switch {
	case os.Geteuid() == 0:
		cmd := exec.Command(self, args...)
		cmd.Env = append(os.Environ(), inNamespaceEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		return run(cmd)
	case exec.Command("sudo", "-n", "true").Run() == nil:
		return run(exec.Command("sudo", append([]string{"-n", self}, args...)...))
	}
	// The binary is copied where the mapped root can enter: a test binary is
	// built in a directory only its builder may.
	dir, err := os.MkdirTemp("", "mvmd-nstest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	copied := filepath.Join(dir, "test.bin")
	if err := copyExecutable(self, copied); err != nil {
		return err
	}
	return run(exec.Command("unshare", append([]string{
		"--user", "--net", "--map-users=auto", "--map-groups=auto", "--setuid=0", "--setgid=0",
		"env", inNamespaceEnv + "=1", copied}, args...)...))
}

func copyExecutable(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func tail(s string, lines int) string {
	parts := bytes.Split([]byte(s), []byte("\n"))
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return string(bytes.Join(parts, []byte("\n")))
}
