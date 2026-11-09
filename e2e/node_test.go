//go:build firecracker_real

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// node is a running mvmd, the built binary, with the Firecracker Driver over the pinned inputs,
// a state directory on an ext4 with fs-verity, and the fixture registry it trusts.
type node struct {
	t        *testing.T
	work     string
	stateDir string
	registry *registry
	client   v1alpha1.RuntimeServiceClient
	cgroups  []string
}

// startNode brings a node up and returns it once its runtime API answers. The test's cleanup
// stops it and everything it started, and removes what it made.
func startNode(t *testing.T) *node {
	t.Helper()
	work, err := os.MkdirTemp("", "mvmd-e2e-")
	require.NoError(t, err)
	// The VMMs run as users of the node's UID range, which pass through this directory.
	require.NoError(t, os.Chmod(work, 0o755))
	n := &node{t: t, work: work}
	t.Cleanup(n.remove)

	n.stateDir = n.mountVerityExt4()
	n.registry = startRegistry(t, work)

	inputs := filepath.Join(work, "inputs")
	require.NoError(t, os.Mkdir(inputs, 0o755))
	stage := func(env string, mode os.FileMode) string {
		return copyFile(t, os.Getenv(env), filepath.Join(inputs, filepath.Base(os.Getenv(env))), mode)
	}
	firecracker := stage(envFirecracker, 0o755)
	kernel := stage(envKernel, 0o644)
	supervisor := stage(envSupervisor, 0o755)
	guestInitrd := copyFile(t, os.Getenv(envGuestInitrd), filepath.Join(inputs, "guest-initrd.cpio"), 0o644)
	converterInitrd := copyFile(t, os.Getenv(envConverterInitrd), filepath.Join(inputs, "converter-initrd.cpio"), 0o644)

	// One cgroup each for the supervisors, the Machines' VMMs and the converter VMs' VMMs, named
	// for this process so that two runs never meet.
	base := "/sys/fs/cgroup/mvmd-e2e-" + strconv.Itoa(os.Getpid())
	supervisors := base + "-supervisors"
	require.NoError(t, os.Mkdir(supervisors, 0o755))
	n.cgroups = []string{supervisors, base, base + "-conversion"}

	socket := filepath.Join(work, "mvmd.sock")
	config := map[string]any{
		"socket":    map[string]any{"path": socket, "mode": "0600"},
		"state_dir": n.stateDir,
		"driver":    "firecracker",
		"disk":      map[string]any{"mkfs": mkfsExt4()},
		"firecracker": map[string]any{
			"binary": firecracker, "binary_sha256": os.Getenv(envFirecrackerSHA),
			"kernel": kernel, "kernel_sha256": os.Getenv(envKernelSHA),
			"initrd": guestInitrd,
		},
		"conversion": map[string]any{
			"kernel": kernel, "kernel_sha256": os.Getenv(envKernelSHA),
			"initrd":    converterInitrd,
			"converter": "e2e-converter",
			"vcpus":     2, "memory_bytes": 512 << 20,
			"output_bytes": 256 << 20, "name_set_bytes": 1 << 20,
			"limit": "5m",
		},
		"supervision":   map[string]any{"binary": supervisor, "cgroup": supervisors, "vmm_cgroup": base},
		"vmm_uid_range": map[string]any{"first": 100000, "count": 1000},
		"pull_timeout":  "8m",
		"boot_timeout":  "2m",
		"call_timeout":  "10m",
	}
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	configFile := filepath.Join(work, "mvmd.json")
	require.NoError(t, os.WriteFile(configFile, raw, 0o600))

	logFile, err := os.Create(filepath.Join(work, "mvmd.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	cmd := exec.Command(os.Getenv(envMvmd), "-config", configFile)
	// The daemon trusts the fixture registry's authority and no other.
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+n.registry.caFile)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	// Closed after the one error it carries, so a receiver that comes after dial took the error
	// (stop) returns at once instead of waiting for a second one.
	go func() { exited <- cmd.Wait(); close(exited) }()
	t.Cleanup(func() { n.stop(cmd, exited) })

	n.client = n.dial(socket, exited)
	return n
}

// dial connects to the runtime API once the daemon serves it, and fails if the daemon exits first.
func (n *node) dial(socket string, exited <-chan error) v1alpha1.RuntimeServiceClient {
	n.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			n.t.Fatalf("mvmd exited before it served: %v\n%s", err, n.log())
		default:
		}
		if _, err := os.Stat(socket); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err := runtimeapi.Dial(ctx, socket)
			cancel()
			if err == nil {
				n.t.Cleanup(func() { _ = conn.Close() })
				return v1alpha1.NewRuntimeServiceClient(conn)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	n.t.Fatalf("mvmd did not serve %s within 60s\n%s", socket, n.log())
	return nil
}

// log is what the daemon wrote, which a failing test prints.
func (n *node) log() string {
	b, err := os.ReadFile(filepath.Join(n.work, "mvmd.log"))
	if err != nil {
		return "no daemon log: " + err.Error()
	}
	return "mvmd log:\n" + string(b)
}

// stop ends the daemon, then every process left in the cgroups it used, and prints the daemon's
// log when the test failed.
func (n *node) stop(cmd *exec.Cmd, exited <-chan error) {
	if n.t.Failed() {
		n.t.Log(n.log())
		n.t.Log(n.conversionLogs())
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	for _, cgroup := range n.cgroups {
		killCgroup(cgroup)
	}
}

// conversionLogs is the tail of what each converter VM left in the state directory: the
// supervisor's log in the VM's state directory, and vmm.log in its run directory, which holds the
// VMM's output and the guest's serial console. They say whether the guest never listened or the
// converter's initrd failed.
func (n *node) conversionLogs() string {
	const tail = 6000
	var b strings.Builder
	dirs, _ := filepath.Glob(filepath.Join(n.stateDir, "conversions", "*"))
	if len(dirs) == 0 {
		return "no converter VM state directory under " + filepath.Join(n.stateDir, "conversions")
	}
	for _, dir := range dirs {
		for _, name := range []string{"supervisor.log", filepath.Join("run", "vmm.log")} {
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(&b, "%s: %v\n", path, err)
				continue
			}
			if len(raw) > tail {
				raw = raw[len(raw)-tail:]
			}
			fmt.Fprintf(&b, "%s (tail):\n%s\n", path, raw)
		}
	}
	return b.String()
}

// remove unmounts the state directory and deletes what the node made. It runs after stop.
func (n *node) remove() {
	for _, cgroup := range n.cgroups {
		removeCgroup(cgroup)
	}
	mount := filepath.Join(n.work, "mnt")
	if _, err := os.Stat(mount); err == nil {
		for i := 0; i < 20; i++ {
			if err := exec.Command("umount", mount).Run(); err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
			if i == 19 {
				_ = exec.Command("umount", "-l", mount).Run()
			}
		}
	}
	_ = os.RemoveAll(n.work)
}

// mountVerityExt4 makes an ext4 with the fs-verity feature in a file and mounts it, as the Image
// Store needs, and returns the state directory in it.
func (n *node) mountVerityExt4() string {
	n.t.Helper()
	image := filepath.Join(n.work, "fs.img")
	mount := filepath.Join(n.work, "mnt")
	for _, step := range [][]string{
		{"truncate", "-s", "2G", image},
		{mkfsExt4(), "-q", "-F", "-b", "4096", "-O", "verity", image},
		{"mkdir", mount},
		{"mount", "-o", "loop", image, mount},
	} {
		out, err := exec.Command(step[0], step[1:]...).CombinedOutput()
		require.NoError(n.t, err, "%s: %s", strings.Join(step, " "), out)
	}
	return filepath.Join(mount, "state")
}

// mkfsExt4 returns the path of mkfs.ext4, which sits in a directory of administrators' tools that
// a PATH can omit.
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

// copyFile copies a file and returns the copy's path.
func copyFile(t *testing.T, from, to string, mode os.FileMode) string {
	t.Helper()
	in, err := os.Open(from)
	require.NoError(t, err)
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, err)
	require.NoError(t, out.Close())
	require.NoError(t, os.Chmod(to, mode))
	return to
}

// killCgroup kills every process in the cgroup and the cgroups under it.
func killCgroup(path string) {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.WriteFile(filepath.Join(p, "cgroup.kill"), []byte("1"), 0o644)
		}
		return nil
	})
}

// removeCgroup removes the cgroup and the ones under it, deepest first, retrying while the kernel
// releases the processes that were killed.
func removeCgroup(path string) {
	var dirs []string
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		for try := 0; try < 40; try++ {
			err := os.Remove(dirs[i])
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// pull publishes an image of the layers to the fixture registry and pulls it through the runtime
// API, the call a client makes, and returns its reference.
func (n *node) pull(name string, layers ...[]byte) ref {
	n.t.Helper()
	r := n.registry.publish(name, []string{"/bin/hello"}, layers...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := n.client.PullImage(ctx, &v1alpha1.PullImageRequest{Image: &v1alpha1.ImageRef{Repository: r.Repository, Digest: r.Digest}})
	require.NoError(n.t, err, "pull %s@%s", r.Repository, r.Digest)
	return r
}

// erofsOf returns the path of the EROFS file the cache holds for the image pulled by the digest,
// read from the cache's OCI layout: the index entry annotated with the digest, its manifest, and
// the manifest's one layer.
func (n *node) erofsOf(digest string) string {
	n.t.Helper()
	images := filepath.Join(n.stateDir, "images")
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	readJSON(n.t, filepath.Join(images, "index.json"), &index)
	for _, entry := range index.Manifests {
		if entry.Annotations["io.mvmd.image.digest"] != digest {
			continue
		}
		var manifest struct {
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}
		readJSON(n.t, blobPath(images, entry.Digest), &manifest)
		require.Len(n.t, manifest.Layers, 1, "the cache's manifest for %s holds one layer", digest)
		return blobPath(images, manifest.Layers[0].Digest)
	}
	n.t.Fatalf("the cache's index lists no image pulled as %s", digest)
	return ""
}

func blobPath(images, digest string) string {
	return filepath.Join(images, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into), "decode %s", path)
}

// tree returns what the EROFS file holds, read by extracting it with the pinned fsck.erofs, as
// root: a whiteout left in the image would extract as a character device, and shows as a special
// file in the tree, never as an absent one. A directory is "name/", a regular file "name=content",
// and anything else "name!mode".
func tree(t *testing.T, erofs string) []string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "extracted")
	require.NoError(t, os.Mkdir(target, 0o755))
	var stderr bytes.Buffer
	cmd := exec.Command(os.Getenv(envFsckErofs), "--extract="+target, erofs)
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "fsck.erofs: %s", stderr.String())

	var entries []string
	require.NoError(t, filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(target, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			entries = append(entries, rel+"/")
		case info.Mode().IsRegular():
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entries = append(entries, fmt.Sprintf("%s=%s", rel, content))
		default:
			entries = append(entries, fmt.Sprintf("%s!%s", rel, info.Mode().Type()))
		}
		return nil
	}))
	return entries
}
