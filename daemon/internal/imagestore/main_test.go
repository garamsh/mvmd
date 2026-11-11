package imagestore_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/garamsh/mvmd/daemon/internal/imagestore"
)

// verityRoot is a directory on a file system that holds fs-verity files, and
// verityReason says why there is none. The tests that need one fail with the
// reason and are never skipped.
var (
	verityRoot   string
	verityReason string
)

// verityEnv names a directory the caller prepared on a file system with
// fs-verity, in place of the search below.
const verityEnv = "MVMD_VERITY_DIR"

func TestMain(m *testing.M) {
	root, cleanup, err := findVerityRoot()
	if err != nil {
		verityReason = err.Error()
	}
	verityRoot = root
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// findVerityRoot returns a directory that can hold fs-verity files, by the
// first of these that works:
//
//   - the directory named by MVMD_VERITY_DIR;
//   - the temporary directory, when its file system has fs-verity;
//   - a loop-mounted ext4 made with the verity feature, through passwordless
//     sudo, which the GitHub-hosted runner provides.
func findVerityRoot() (string, func(), error) {
	nothing := func() {}
	var tried []string
	if dir := os.Getenv(verityEnv); dir != "" {
		if err := supportsVerity(dir); err != nil {
			return "", nothing, fmt.Errorf("%s=%s: %w", verityEnv, dir, err)
		}
		return dir, nothing, nil
	}
	err := supportsVerity(os.TempDir())
	if err == nil {
		return os.TempDir(), nothing, nil
	}
	tried = append(tried, fmt.Sprintf("%s: %v", os.TempDir(), err))
	dir, cleanup, err := mountVerityExt4()
	if err != nil {
		tried = append(tried, "a loop-mounted ext4 with verity: "+err.Error())
		return "", nothing, fmt.Errorf("no file system with fs-verity is available (%s); set %s to a directory on one", strings.Join(tried, "; "), verityEnv)
	}
	return dir, cleanup, nil
}

// supportsVerity reports whether a new image cache in a fresh directory of dir
// opens, which is what it does when fs-verity can be enabled there.
func supportsVerity(dir string) error {
	probe, err := os.MkdirTemp(dir, "verity-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probe)
	_, err = imagestore.New(probe, testClock)
	return err
}

// mountVerityExt4 makes a small ext4 with the verity feature in a file and
// mounts it through sudo, owned by the user running the tests.
func mountVerityExt4() (string, func(), error) {
	work, err := os.MkdirTemp("", "mvmd-verity-")
	if err != nil {
		return "", nil, err
	}
	image := filepath.Join(work, "fs.img")
	mount := filepath.Join(work, "mnt")
	steps := [][]string{
		{"truncate", "-s", "256M", image},
		{mkfsExt4(), "-q", "-F", "-b", "4096", "-O", "verity", image},
		{"mkdir", mount},
		{"sudo", "-n", "mount", "-o", "loop", image, mount},
		{"sudo", "-n", "chown", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), mount},
	}
	mounted := false
	cleanup := func() {
		if mounted {
			_ = exec.Command("sudo", "-n", "umount", mount).Run()
		}
		_ = os.RemoveAll(work)
	}
	for _, step := range steps {
		if out, err := exec.Command(step[0], step[1:]...).CombinedOutput(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("%s: %w: %s", strings.Join(step, " "), err, strings.TrimSpace(string(out)))
		}
		if step[0] == "sudo" && step[2] == "mount" {
			mounted = true
		}
	}
	if err := supportsVerity(mount); err != nil {
		cleanup()
		return "", nil, err
	}
	return mount, cleanup, nil
}

// mkfsExt4 returns the path of mkfs.ext4, which sits in a directory of
// administrators' tools that an unprivileged PATH can omit.
func mkfsExt4() string {
	if p, err := exec.LookPath("mkfs.ext4"); err == nil {
		return p
	}
	for _, p := range []string{"/sbin/mkfs.ext4", "/usr/sbin/mkfs.ext4"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "mkfs.ext4"
}

// verityDir returns a fresh directory on a file system with fs-verity, and
// fails the test where there is none.
func verityDir(t *testing.T) string {
	t.Helper()
	if verityRoot == "" {
		t.Fatalf("this test needs a file system with fs-verity: %s", verityReason)
	}
	dir, err := os.MkdirTemp(verityRoot, "cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
