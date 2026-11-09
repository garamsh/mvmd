package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// bindMount is one host file or directory made visible at a path in the VMM's
// root.
type bindMount struct {
	host     string
	path     string
	readOnly bool
	// executable keeps the mount from being noexec.
	executable bool
	// sha256 is the digest in hex the file must have, or empty.
	sha256 string
}

// mountsFor lists what the root holds beside /dev: the VMM executable and the
// host files the Driver names, each at its host path; the boot description's
// files at the paths it gives them, with the mode each is marked; and the run
// directory, read-write, at its path. A path given twice for one host file in
// one mode is mounted once. The description is refused for two host files at
// one path, or one path in two modes, or a path inside another path, since a
// mount inside a read-only mount cannot be made, or a path that is the root or
// under /dev, where the root's own /dev replaces it.
func mountsFor(req *pb.StartRequest) ([]bindMount, error) {
	p := req.GetProcess()
	var all []bindMount
	all = append(all, bindMount{host: p.GetExecutable(), path: p.GetExecutable(), readOnly: true, executable: true, sha256: p.GetExecutableSha256()})
	for _, f := range p.GetBootFiles() {
		all = append(all, bindMount{host: f.GetPath(), path: f.GetInPath(), readOnly: f.GetReadOnly(), sha256: f.GetSha256()})
	}
	for _, f := range p.GetReadFiles() {
		all = append(all, bindMount{host: f, path: f, readOnly: true})
	}
	all = append(all, bindMount{host: req.GetRunDir(), path: req.GetRunDirInPath()})

	byPath := map[string]bindMount{}
	for _, m := range all {
		if m.path == "/" || m.path == "/dev" || strings.HasPrefix(m.path, "/dev/") {
			return nil, fmt.Errorf("%s cannot be the path of %s in the root", m.path, m.host)
		}
		if have, ok := byPath[m.path]; ok {
			switch {
			case have.host != m.host:
				return nil, fmt.Errorf("%s and %s are both given the path %s", have.host, m.host, m.path)
			case have.readOnly != m.readOnly:
				return nil, fmt.Errorf("%s is given both read-only and read-write", m.path)
			case have.sha256 != "" && m.sha256 != "" && have.sha256 != m.sha256:
				return nil, fmt.Errorf("%s is given two digests", m.path)
			}
			have.executable = have.executable || m.executable
			if have.sha256 == "" {
				have.sha256 = m.sha256
			}
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

// afterHash is called between the hash of a pinned file and the check of it, the
// instant at which a file replaced at the path would be the file a second open
// of the path found. It is a seam for the test that replaces the file there.
var afterHash = func() {}

// present opens the host path once and, for a pinned file, hashes what it
// opened. The caller mounts the file it is returned and never the path again,
// so a file replaced at the path after the hash is not the file the VMM is
// given (ADR 0019). A file that is not pinned is opened for its place in the
// file system only.
func present(host, sha256Hex string) (*os.File, error) {
	if sha256Hex == "" {
		fd, err := unix.Open(host, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: host, Err: err}
		}
		return os.NewFile(uintptr(fd), host), nil
	}
	f, err := os.Open(host)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", host)
	}
	var got string
	if err == nil {
		h := sha256.New()
		if _, err = io.Copy(h, f); err == nil {
			got = hex.EncodeToString(h.Sum(nil))
		}
	}
	afterHash()
	if err == nil && got != sha256Hex {
		err = fmt.Errorf("%s has digest %s, not the pinned %s", host, got, sha256Hex)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// bind makes the host file or directory visible at the mount's path in newroot.
// The mount is cloned from the descriptor present returned, with open_tree and
// move_mount, which is what makes it that file and no other.
func bind(newroot string, m bindMount) error {
	src, err := present(m.host, m.sha256)
	if err != nil {
		return fmt.Errorf("present %s: %w", m.host, err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", m.host, err)
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
	tree, err := unix.OpenTree(int(src.Fd()), "", unix.OPEN_TREE_CLONE|unix.AT_EMPTY_PATH|unix.O_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone the mount of %s: %w", m.host, err)
	}
	err = unix.MoveMount(tree, "", unix.AT_FDCWD, dst, unix.MOVE_MOUNT_F_EMPTY_PATH)
	unix.Close(tree)
	if err != nil {
		return fmt.Errorf("bind %s at %s: %w", m.host, m.path, err)
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
