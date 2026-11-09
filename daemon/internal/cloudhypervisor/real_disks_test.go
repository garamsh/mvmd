//go:build cloud_hypervisor_real

package cloudhypervisor_test

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
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// vmmRequest sends a request straight to the VMM's API socket, which the Driver
// does not offer: what a test reads of the VMM and the one request it must not
// make through the Driver.
func vmmRequest(t *testing.T, socket, method, path, body string) (int, []byte) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return unixsock.DialContext(ctx, socket)
		},
	}}
	req, err := http.NewRequest(method, "http://cloud-hypervisor/api/v1"+path, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// disksOf is the disks the VMM reports having, by ID, with their paths.
func disksOf(t *testing.T, socket string) map[string]string {
	t.Helper()
	status, raw := vmmRequest(t, socket, "GET", "/vm.info", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var info struct {
		Config struct {
			Disks []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"disks"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(raw, &info))
	out := map[string]string{}
	for _, d := range info.Config.Disks {
		out[d.ID] = d.Path
	}
	return out
}

// deviceOf is the name the Driver gives a disk's device, which it derives from
// the disk's ID.
var deviceOf = map[machine.DiskID]string{
	"hot":   "disk_7f5d1618e7d28cc7bf32788672be04c24877d9a8911f65fef8e3e013c9cbc373",
	"hotro": "disk_b704747b4231d0e7c7e8fc3e47ff82ea61c73b9eda19e4459c42cafa3a2a4ddf",
	"root":  "disk_4813494d137e1631bba301d5acab6e7bb7aa74ce1185d456565ef51d737677b2",
}

// checkVMM is what only Cloud Hypervisor can show of a Machine, which
// drivertest hands to it at each checkpoint: the disks it reports having, the
// links those hot-plugged ones have in the run directory, and its own sockets.
// The VMM opens the file when the disk is added, so that it accepts the path in
// the run directory is what shows the link is in the VMM's root.
func checkVMM(t *testing.T, at drivertest.Checkpoint, m drivertest.Machine) {
	t.Helper()
	sock := filepath.Join(m.RunDir, "api.sock")
	switch at {
	case drivertest.Booted:
		assert.Len(t, disksOf(t, sock), len(m.Devices), "the boot's devices are the VMM's disks")
	case drivertest.Attached:
		disks := disksOf(t, sock)
		assert.Len(t, disks, len(m.Devices), "every disk the Machine holds is a disk of the VMM")
		var plugged machine.BlockDevice
		for _, dev := range m.Devices {
			name, hot := deviceOf[dev.Disk]
			if !hot || dev.Disk == "root" {
				continue
			}
			plugged = dev
			assert.Equal(t, "/run/"+name, disks[name], "the attached disk is at its link in the run directory")
			link, err := os.Stat(filepath.Join(m.RunDir, name))
			require.NoError(t, err)
			file, err := os.Stat(dev.Path)
			require.NoError(t, err)
			assert.True(t, os.SameFile(file, link), "the link of %s is the disk's file", dev.Disk)
		}
		// Control: the VMM opens the file at the path it is given, and a path
		// the root does not hold is refused, which is why the link exists.
		status, raw := vmmRequest(t, sock, "PUT", "/vm.add-disk",
			`{"id":"stray","path":"`+plugged.Path+`","readonly":true,"image_type":"Raw"}`)
		assert.GreaterOrEqual(t, status, 400, "a host path is not in the VMM's root: %s", raw)
	case drivertest.Detached:
		require.Len(t, m.Devices, 2, "control: the image and the root disk are what is left")
		assert.Equal(t, map[string]string{"disk0": "/disk/0", deviceOf["root"]: "/disk/1"}, disksOf(t, sock), "only the image and the root disk remain")
		for _, disk := range []machine.DiskID{"hot", "hotro"} {
			_, err := os.Lstat(filepath.Join(m.RunDir, deviceOf[disk]))
			assert.ErrorIs(t, err, os.ErrNotExist, "the link of %s is gone", disk)
		}
		// The detach returns once the guest has ejected, so the device tree
		// holds none of the disks detached, the one given at boot ("given")
		// included.
		const given = "disk_5b729e0f619797fd61108a4bb177273222ad9ac5538299c8386f061e29046e60"
		status, raw := vmmRequest(t, sock, "GET", "/vm.info", "")
		require.Equal(t, http.StatusOK, status, string(raw))
		for _, name := range []string{deviceOf["hot"], deviceOf["hotro"], given} {
			assert.NotContains(t, string(raw), `"`+name+`":`, "a detached device is still in the VMM's device tree")
		}
	}
}
