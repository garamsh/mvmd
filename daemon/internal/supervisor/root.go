package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// bindMount is one host path made visible at its own path in the VMM's root.
type bindMount struct {
	path     string
	readOnly bool
	// executable keeps the mount from being noexec.
	executable bool
}

// mountsFor lists what the root holds beside /dev: the VMM executable, the boot
// description's files with the mode each is marked, the host files the Driver
// names, read-only, and the run directory, read-write. A path listed twice
// with one mode is mounted once; with two modes, or inside another path, the
// description is refused, since a mount inside a read-only mount cannot be made.
func mountsFor(req *pb.StartRequest) ([]bindMount, error) {
	p := req.GetProcess()
	var all []bindMount
	all = append(all, bindMount{path: p.GetExecutable(), readOnly: true, executable: true})
	for _, f := range p.GetBootFiles() {
		all = append(all, bindMount{path: f.GetPath(), readOnly: f.GetReadOnly()})
	}
	for _, f := range p.GetReadFiles() {
		all = append(all, bindMount{path: f, readOnly: true})
	}
	all = append(all, bindMount{path: req.GetRunDir()})

	byPath := map[string]bindMount{}
	for _, m := range all {
		if have, ok := byPath[m.path]; ok {
			if have.readOnly != m.readOnly {
				return nil, fmt.Errorf("%s is given both read-only and read-write", m.path)
			}
			have.executable = have.executable || m.executable
			byPath[m.path] = have
			continue
		}
		byPath[m.path] = m
	}
	var out []bindMount
	for _, m := range byPath {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	for _, a := range out {
		for _, b := range out {
			if a.path != b.path && strings.HasPrefix(b.path, a.path+"/") {
				return nil, fmt.Errorf("%s is inside %s", b.path, a.path)
			}
		}
	}
	return out, nil
}

// buildRoot makes the VMM's root and enters it: a new tmpfs at newroot that
// holds only the bind mounts and the device nodes, a pivot into it, and the
// host's root unmounted. The calling thread must be in a mount namespace of its
// own, and must still hold the capabilities the mounts need. No /proc or /sys
// is mounted.
func buildRoot(newroot string, req *pb.StartRequest) error {
	mounts, err := mountsFor(req)
	if err != nil {
		return err
	}
	// Nothing mounted here may reach the host's namespace.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the mounts private: %w", err)
	}
	if err := os.MkdirAll(newroot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", newroot, err)
	}
	if err := unix.Mount("tmpfs", newroot, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("mount the root: %w", err)
	}
	for _, m := range mounts {
		if err := bind(newroot, m); err != nil {
			return err
		}
	}
	if err := makeDevices(newroot, req.GetProcess().GetDeviceNodes(), int(req.GetUid()), int(req.GetGid())); err != nil {
		return err
	}
	if err := os.Chdir(newroot); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot into the root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount the host's root: %w", err)
	}
	return os.Chdir("/")
}

// bind makes the host path visible at the same path in newroot.
func bind(newroot string, m bindMount) error {
	st, err := os.Stat(m.path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", m.path, err)
	}
	dst := filepath.Join(newroot, m.path)
	if st.IsDir() {
		err = os.MkdirAll(dst, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
		var f *os.File
		if f, err = os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			err = f.Close()
		}
	}
	if err != nil {
		return fmt.Errorf("make the mount point of %s: %w", m.path, err)
	}
	if err := unix.Mount(m.path, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", m.path, err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_NOSUID | unix.MS_NODEV)
	if m.readOnly {
		flags |= unix.MS_RDONLY
	}
	if !m.executable {
		flags |= unix.MS_NOEXEC
	}
	if err := unix.Mount("", dst, "", flags, ""); err != nil {
		return fmt.Errorf("restrict the mount of %s: %w", m.path, err)
	}
	return nil
}

// makeDevices mounts a /dev of its own in newroot and creates in it each device
// node the Driver names, with the host node's number, owned by the VMM's user.
func makeDevices(newroot string, nodes []string, uid, gid int) error {
	dev := filepath.Join(newroot, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return fmt.Errorf("create /dev: %w", err)
	}
	// Not nodev: the nodes made here are the point of it.
	if err := unix.Mount("tmpfs", dev, "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC, "mode=0755,size=64k"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	for _, n := range nodes {
		var st unix.Stat_t
		if err := unix.Stat(n, &st); err != nil {
			return fmt.Errorf("stat the device node %s: %w", n, err)
		}
		kind := st.Mode & unix.S_IFMT
		if kind != unix.S_IFCHR && kind != unix.S_IFBLK {
			return fmt.Errorf("%s is not a device node", n)
		}
		dst := filepath.Join(newroot, n)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("make the directory of %s: %w", n, err)
		}
		if err := unix.Mknod(dst, kind|0o600, int(st.Rdev)); err != nil {
			return fmt.Errorf("make the device node %s: %w", n, err)
		}
		if err := unix.Chown(dst, uid, gid); err != nil {
			return fmt.Errorf("give %s to the Machine's user: %w", n, err)
		}
	}
	return nil
}

// bringUpLoopback brings up lo in the calling network namespace, which a new
// one has down.
func bringUpLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open a socket: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("read the flags of lo: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring up lo: %w", err)
	}
	return nil
}
