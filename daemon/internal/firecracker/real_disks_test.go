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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/drivertest"
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

// driveOf is the drive Firecracker names a disk by, which the Driver derives
// from the disk's ID.
var driveOf = map[machine.DiskID]string{
	"hot":   "disk_7f5d1618e7d28cc7bf32788672be04c24877d9a8911f65fef8e3e013c9cbc373",
	"hotro": "disk_b704747b4231d0e7c7e8fc3e47ff82ea61c73b9eda19e4459c42cafa3a2a4ddf",
	"root":  "disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2",
}

// checkVMM is what only Firecracker can show of a Machine, which drivertest
// hands to it at each checkpoint: the drives it reports having, the links
// those hot-plugged ones have in the run directory, and its own sockets.
// Firecracker opens the file when the drive is put, so that it accepts the path
// in the run directory is what shows the link is in the VMM's root.
func checkVMM(t *testing.T, at drivertest.Checkpoint, m drivertest.Machine) {
	t.Helper()
	sock := filepath.Join(m.RunDir, "api.sock")
	switch at {
	case drivertest.Booted:
		assert.Len(t, drivesOf(t, sock), len(m.Devices), "the boot's devices are the VMM's drives")
	case drivertest.Attached:
		drives := drivesOf(t, sock)
		assert.Len(t, drives, len(m.Devices), "every disk the Machine holds is a drive")
		var plugged machine.BlockDevice
		for _, dev := range m.Devices {
			name, hot := driveOf[dev.Disk]
			if !hot || dev.Disk == "root" {
				continue
			}
			plugged = dev
			assert.Equal(t, "/run/"+name, drives[name], "the attached disk is a drive at its link in the run directory")
			link, err := os.Stat(filepath.Join(m.RunDir, name))
			require.NoError(t, err)
			file, err := os.Stat(dev.Path)
			require.NoError(t, err)
			assert.True(t, os.SameFile(file, link), "the link of %s is the disk's file", dev.Disk)
		}
		// Control: Firecracker opens the file at the path it is given, and a
		// path the root does not hold is refused, which is why the link exists.
		status, raw := vmmRequest(t, sock, "PUT", "/drives/stray",
			`{"drive_id":"stray","path_on_host":"`+plugged.Path+`","is_root_device":false,"is_read_only":true}`)
		assert.GreaterOrEqual(t, status, 400, "a host path is not in the VMM's root: %s", raw)
	case drivertest.Detached:
		require.Len(t, m.Devices, 2, "control: the image and the root disk are what is left")
		assert.Equal(t, map[string]string{"disk0": "/disk/0", driveOf["root"]: "/disk/1"}, drivesOf(t, sock), "only the image and the root disk remain")
		for _, disk := range []machine.DiskID{"hot", "hotro"} {
			_, err := os.Lstat(filepath.Join(m.RunDir, driveOf[disk]))
			assert.ErrorIs(t, err, os.ErrNotExist, "the link of %s is gone", disk)
		}
	case drivertest.Restored:
		_, err := os.Stat(filepath.Join(m.RunDir, "vsock.sock"))
		assert.NoError(t, err, "%s has its own vsock socket", m.ID)
	}
}
