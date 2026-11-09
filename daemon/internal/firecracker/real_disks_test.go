//go:build firecracker_real

package firecracker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/channel"
	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// vmmRequest sends a request straight to the VMM's API socket, which the Driver
// does not offer: what a test reads of the VMM and the one request it must not
// make through the Driver.
func vmmRequest(t *testing.T, socket, method, path, body string) (int, []byte) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequest(method, "http://firecracker"+path, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// drivesOf is the drives the VMM reports having, by ID, with their paths.
func drivesOf(t *testing.T, socket string) map[string]string {
	t.Helper()
	status, raw := vmmRequest(t, socket, "GET", "/vm/config", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var cfg struct {
		Drives []struct {
			ID   string `json:"drive_id"`
			Path string `json:"path_on_host"`
		} `json:"drives"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	out := map[string]string{}
	for _, d := range cfg.Drives {
		out[d.ID] = d.Path
	}
	return out
}

// A disk is hot-plugged into a booted Machine and unplugged again, and so is
// one the boot gave it. Firecracker opens the file when the drive is put, so
// that it accepts the path in the run directory is what shows the link is in
// the VMM's root.
func TestReal_ADiskIsAttachedToARunningMachineAndDetachedAgain(t *testing.T) {
	// The Unix socket paths bound the length of this directory.
	dir, err := os.MkdirTemp("", "fcdisk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	run := filepath.Join(dir, "run")
	require.NoError(t, os.Mkdir(run, 0o700))

	d, err := firecracker.New(firecracker.Config{
		Binary: input(t, "MVMD_FIRECRACKER"), BinarySHA256: input(t, "MVMD_FIRECRACKER_SHA256"),
		Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
		Initrd: input(t, "MVMD_INITRD"),
		Dirs:   runDirs{run},
	})
	require.NoError(t, err)
	require.Contains(t, d.Info().Capabilities, machine.CapabilityDiskHotplug)

	file := func(name string, readOnly bool) machine.BlockDevice {
		path := filepath.Join(dir, name+".img")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Truncate(path, 16<<20))
		return machine.BlockDevice{Disk: machine.DiskID(name), Path: path, ReadOnly: readOnly}
	}
	image, root, given := file("image", true), file("root", false), file("given", false)
	image.Disk = ""
	spec := machine.BootSpec{
		Devices:   []machine.BlockDevice{image, root, given},
		Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}, RunDir: run,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startVMM(t, p.For(spec), run)
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := channel.NewGuest(d)
	answers := func() bool {
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		defer ccancel()
		return guest.Health(cctx, "m1") == nil
	}
	require.Eventually(t, answers, time.Minute, 500*time.Millisecond, "the Guest Agent never answered")
	sock := filepath.Join(run, "api.sock")
	before := drivesOf(t, sock)
	require.Len(t, before, 3, "control: the boot's three devices are the VMM's drives")

	hot, hotRO := file("hot", false), file("hotro", true)
	for _, dev := range []machine.BlockDevice{hot, hotRO} {
		require.NoError(t, d.AttachDisk(ctx, "m1", dev), dev.Disk)
	}
	after := drivesOf(t, sock)
	assert.Len(t, after, 5)
	assert.Equal(t, "/run/disk_7f5d1618e7d28cc7bf32788672be04c24877d9a8911f65fef8e3e013c9cbc373", after["disk_7f5d1618e7d28cc7bf32788672be04c24877d9a8911f65fef8e3e013c9cbc373"], "the attached disk is a drive at its link in the run directory")
	assert.Equal(t, "/run/disk_b704747b4231d0e7c7e8fc3e47ff82ea61c73b9eda19e4459c42cafa3a2a4ddf", after["disk_b704747b4231d0e7c7e8fc3e47ff82ea61c73b9eda19e4459c42cafa3a2a4ddf"])
	hotLink, err := os.Stat(filepath.Join(run, "disk_7f5d1618e7d28cc7bf32788672be04c24877d9a8911f65fef8e3e013c9cbc373"))
	require.NoError(t, err)
	hotFile, err := os.Stat(hot.Path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(hotFile, hotLink))
	assert.True(t, answers(), "the guest still answers with the disks attached")

	// Control: Firecracker opens the file at the path it is given, and a path
	// the root does not hold is refused, which is why the link exists.
	status, raw := vmmRequest(t, sock, "PUT", "/drives/stray",
		`{"drive_id":"stray","path_on_host":"`+hot.Path+`","is_root_device":false,"is_read_only":true}`)
	assert.GreaterOrEqual(t, status, 400, "a host path is not in the VMM's root: %s", raw)

	require.NoError(t, d.DetachDisk(ctx, "m1", "hot"))
	require.NoError(t, d.DetachDisk(ctx, "m1", "hotro"))
	require.NoError(t, d.DetachDisk(ctx, "m1", "given"))
	assert.Equal(t, map[string]string{"disk0": "/disk/0", "disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2": "/disk/1"}, drivesOf(t, sock), "only the image and the root disk remain")
	for _, name := range []string{"disk_7f5d1618e7d28cc7bf32788672be04c24877d9a8911f65fef8e3e013c9cbc373", "disk_b704747b4231d0e7c7e8fc3e47ff82ea61c73b9eda19e4459c42cafa3a2a4ddf"} {
		_, err := os.Lstat(filepath.Join(run, name))
		assert.ErrorIs(t, err, os.ErrNotExist, "the link of %s is gone", name)
	}
	for _, dev := range []machine.BlockDevice{hot, hotRO, given} {
		_, err := os.Stat(dev.Path)
		assert.NoError(t, err, "the file of %s is untouched", dev.Disk)
	}
	assert.True(t, answers(), "the guest still answers after the detach")
}
