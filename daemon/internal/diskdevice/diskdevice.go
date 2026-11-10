// Package diskdevice names the block device a VMM holds for a disk, and removes
// the link a hot-plugged disk leaves in the run directory. Both VMM Drivers use
// it, so they name a device and clear a link the same way.
package diskdevice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Name names a block device to the VMM. A device that holds a disk is named by
// it, so a detach finds the device whether the disk was given at boot or
// attached later, and a Machine's image, which holds none, by its position. The
// name is also a file name in the run directory, so it is the hex of the ID's
// SHA-256: the same length for any ID, within the letters, digits and
// underscores Firecracker accepts of an ID, free of the "__" prefix Cloud
// Hypervisor reserves for its own, and distinct for two IDs.
func Name(position int, disk machine.DiskID) string {
	if disk == "" {
		return fmt.Sprintf("disk%d", position)
	}
	sum := sha256.Sum256([]byte(disk))
	return "disk_" + hex.EncodeToString(sum[:])
}

// RemoveLink removes the link of an attached disk, which is absent for a disk
// given at boot.
func RemoveLink(link string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", link, err)
	}
	return nil
}
