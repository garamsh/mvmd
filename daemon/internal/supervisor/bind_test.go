package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// These tests build a root, which needs mount rights: they run as root in a
// mount namespace of their own, under passwordless sudo when the test binary is
// not root, and fail where neither is available, as internal/supervision's do.
// In a pipeline run they are CI's.

type bindArgs struct {
	Host, In, Digest, Replacement string
	ByPath                        bool
}

func init() {
	cases["bind"] = func(raw []byte) (map[string]string, error) {
		var a bindArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		// The file at the host path is replaced between the hash and the mount.
		afterHash = func() {
			if a.Replacement == "" {
				return
			}
			tmp := a.Host + ".replacement"
			if err := os.WriteFile(tmp, []byte(a.Replacement), 0o644); err == nil {
				_ = os.Rename(tmp, a.Host)
			}
		}
		newroot := filepath.Join(os.TempDir(), "bind-newroot")
		res := map[string]string{}
		read := func(path string) {
			b, err := os.ReadFile(path)
			if err != nil {
				res["read"] = errName(err)
				return
			}
			res["read"] = string(b)
		}
		if !a.ByPath {
			run := filepath.Join(os.TempDir(), "bind-run")
			if err := os.MkdirAll(run, 0o755); err != nil {
				return nil, err
			}
			req := &pb.StartRequest{
				Process: &pb.Process{
					Executable: os.Args[0],
					BootFiles:  []*pb.BlockDevice{{Path: a.Host, InPath: a.In, ReadOnly: true, Sha256: a.Digest}},
				},
				RunDir: run, RunDirInPath: "/run",
			}
			if err := buildRoot(newroot, req); err != nil {
				// The kernel refuses to mount a file that was unlinked, which a
				// replaced file is: the start fails and nothing is mounted.
				res["error"] = err.Error()
				return res, nil
			}
			read(a.In)
			return res, nil
		}
		// The control: what bind did before it cloned the mount from the
		// descriptor, resolving the path again after present.
		if err := os.MkdirAll(newroot, 0o700); err != nil {
			return nil, err
		}
		if err := unix.Mount("tmpfs", newroot, "tmpfs", 0, ""); err != nil {
			return nil, err
		}
		f, err := present(a.Host, a.Digest)
		if err != nil {
			return nil, err
		}
		f.Close()
		dst := filepath.Join(newroot, "file")
		if err := os.WriteFile(dst, nil, 0o644); err != nil {
			return nil, err
		}
		if err := unix.Mount(a.Host, dst, "", unix.MS_BIND, ""); err != nil {
			return nil, err
		}
		read(dst)
		return res, nil
	}
}

// inMountNamespace runs the case in a child of this test binary in a mount
// namespace of its own, as root.
func inMountNamespace(t *testing.T, name string, args any) map[string]string {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	argv := []string{"unshare", "--mount", os.Args[0], "-test.run=^$"}
	env := []string{caseEnv + "=" + name, argsEnv + "=" + string(raw)}
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), env...)
	} else {
		cmd = exec.Command("sudo", append(append([]string{"-n", "env"}, env...), argv...)...)
	}
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Fatalf("the case failed: %v\n%s", err, ee.Stderr)
	}
	require.NoError(t, err, "this test needs root or passwordless sudo, and `unshare`")
	var res map[string]string
	require.NoError(t, json.Unmarshal(out, &res), string(out))
	return res
}

// The file mounted in the root is the file the hash saw, and a file put at the
// host path after the hash is never mounted (ADR 0019). Without a replacement
// the pinned file is mounted and read; with one, the bind fails and the
// replacement is not read. The control, a bind that resolves the path again,
// reads the replacement from the same setup, so the test sees the window were
// it open.
func TestBind_TheFileMountedIsTheFileHashed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755))
	host := filepath.Join(dir, "kernel")
	require.NoError(t, os.WriteFile(host, []byte("the pinned kernel"), 0o644))
	args := bindArgs{Host: host, In: "/boot/kernel", Digest: sumOf([]byte("the pinned kernel"))}

	res := inMountNamespace(t, "bind", args)
	assert.Equal(t, "the pinned kernel", res["read"], "setup: with no replacement the pinned file is mounted and read")
	assert.Empty(t, res["error"])

	args.Replacement = "a replacement"
	res = inMountNamespace(t, "bind", args)
	assert.NotEqual(t, "a replacement", res["read"], "the replacement is not what the VMM reads")
	assert.Empty(t, res["read"], "nothing is mounted")
	assert.Contains(t, res["error"], "bind", "the start fails at the mount")
	now, err := os.ReadFile(host)
	require.NoError(t, err)
	require.Equal(t, "a replacement", string(now), "setup: the host path now holds the replacement")

	// The replacement was renamed in by the root child, so the file is root's
	// and this process, which is not root under sudo, cannot open it to write.
	// The directory is this process's: unlink the file and create it again.
	require.NoError(t, os.Remove(host))
	require.NoError(t, os.WriteFile(host, []byte("the pinned kernel"), 0o644))
	args.ByPath = true
	ctl := inMountNamespace(t, "bind", args)
	assert.Equal(t, "a replacement", ctl["read"], "control: a bind that resolves the path after the hash mounts the replacement")
}
