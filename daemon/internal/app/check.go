package app

import (
	"fmt"
	"maps"
	"slices"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/pinnedfile"
	"github.com/garamsh/mvmd/daemon/internal/supervisor"
)

// CheckHost checks what a start would check of the host and the files the
// configuration names, and needs no state: each pinned file has its digest, the
// kernel takes the confinement the supervisor needs, and mkfs.ext4 runs. It
// opens no database and no socket, makes no directory and starts nothing. It
// returns what it covered, as a line, and the first thing that is wrong.
func (c Config) CheckHost() (string, error) {
	files := 0
	check := func(what, path, sum string) error {
		files++
		return pinnedfile.Check(what, path, sum)
	}
	if v, ok := c.vmm(); ok {
		if err := check("the "+c.Driver+" binary", v.Binary, v.BinarySHA256); err != nil {
			return "", err
		}
		if err := check("the guest initrd", v.Initrd, v.InitrdSHA256); err != nil {
			return "", err
		}
		for _, name := range slices.Sorted(maps.Keys(v.Kernels)) {
			if err := check(fmt.Sprintf("the kernel %q", name), v.Kernels[name].Path, v.Kernels[name].SHA256); err != nil {
				return "", err
			}
		}
		for _, name := range slices.Sorted(maps.Keys(v.BootSets)) {
			b := v.BootSets[name]
			if err := check(fmt.Sprintf("the kernel of boot set %q", name), b.Kernel.Path, b.Kernel.SHA256); err != nil {
				return "", err
			}
			if err := check(fmt.Sprintf("the initrd of boot set %q", name), b.Initrd.Path, b.Initrd.SHA256); err != nil {
				return "", err
			}
		}
	}
	if cv := c.Conversion; cv != nil {
		if err := check("the converter's kernel", cv.Kernel, cv.KernelSHA256); err != nil {
			return "", err
		}
		if err := check("the converter's initrd", cv.Initrd, cv.InitrdSHA256); err != nil {
			return "", err
		}
	}
	checks := fmt.Sprintf("%d pinned file(s)", files)
	if c.Supervision != nil {
		if err := supervisor.CheckHost(); err != nil {
			return "", err
		}
		checks += ", the Landlock ABI"
	}
	if c.Disk != nil {
		if err := diskfile.CheckMKFS(c.Disk.MKFS); err != nil {
			return "", err
		}
		checks += ", mkfs.ext4"
	}
	return "checked " + checks, nil
}
