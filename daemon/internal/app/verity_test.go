package app_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/imagestore"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// The Image Store of the Firecracker configuration needs a file system with
// fs-verity under the state directory. Finding one costs a loop mount, so the
// search runs when the first test needs one. This duplicates
// imagestore/main_test.go: a _test.go file is not importable, and the helper
// is not worth a package of its own for two users.

const verityEnv = "MVMD_VERITY_DIR"

var (
	verityOnce    sync.Once
	verityRoot    string
	verityReason  string
	verityCleanup = func() {}
)

func TestMain(m *testing.M) {
	code := m.Run()
	verityCleanup()
	os.Exit(code)
}

// verityDir returns a fresh directory on a file system with fs-verity, and
// fails the test where there is none; it is never skipped.
func verityDir(t *testing.T) string {
	t.Helper()
	verityOnce.Do(func() {
		root, cleanup, err := findVerityRoot()
		if err != nil {
			verityReason = err.Error()
			return
		}
		verityRoot, verityCleanup = root, cleanup
	})
	if verityRoot == "" {
		t.Fatalf("this test needs a file system with fs-verity: %s", verityReason)
	}
	dir, err := os.MkdirTemp(verityRoot, "node-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// findVerityRoot returns a directory that can hold fs-verity files, by the
// first of these that works: the directory named by MVMD_VERITY_DIR; the
// temporary directory; a loop-mounted ext4 made with the verity feature,
// through passwordless sudo.
func findVerityRoot() (string, func(), error) {
	nothing := func() {}
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
	tried := fmt.Sprintf("%s: %v", os.TempDir(), err)
	dir, cleanup, err := mountVerityExt4()
	if err != nil {
		return "", nothing, fmt.Errorf("no file system with fs-verity is available (%s; a loop-mounted ext4 with verity: %v); set %s to a directory on one", tried, err, verityEnv)
	}
	return dir, cleanup, nil
}

// supportsVerity reports whether a new Image Store in a fresh directory of dir
// opens, which is what it does when fs-verity can be enabled there.
func supportsVerity(dir string) error {
	probe, err := os.MkdirTemp(dir, "verity-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probe)
	_, err = imagestore.New(probe, memory.NewWallClock(time.Now()))
	return err
}

// mountVerityExt4 makes a small ext4 with the verity feature in a file and
// mounts it through sudo, owned by the user running the tests.
func mountVerityExt4() (string, func(), error) {
	work, err := os.MkdirTemp("", "mvmd-verity-")
	if err != nil {
		return "", nil, err
	}
	img := filepath.Join(work, "fs.img")
	mount := filepath.Join(work, "mnt")
	steps := [][]string{
		{"truncate", "-s", "256M", img},
		{mkfsExt4(), "-q", "-F", "-b", "4096", "-O", "verity", img},
		{"mkdir", mount},
		{"sudo", "-n", "mount", "-o", "loop", img, mount},
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
