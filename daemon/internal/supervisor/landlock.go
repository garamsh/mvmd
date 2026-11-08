package supervisor

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// landlockABIRequired is the Landlock ABI the confinement needs: the one that
// scopes abstract Unix sockets and signals (ADR 0015).
const landlockABIRequired = 6

// handledFS is every file system right ABI 6 knows, so that a right no rule
// grants is denied.
const handledFS = 1<<16 - 1

// fileRights are the rights a rule on something that is not a directory may
// carry; the kernel refuses a rule that names another.
const fileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

// The access sets a path is granted, by what it is in the root (ADR 0015).
const (
	// accessExecutable lets the process run the file and read it.
	accessExecutable = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE
	// accessRead is a read-only file or directory.
	accessRead = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	// accessFile is a boot file the process may write.
	accessFile = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE
	// accessDevice is a device node the process opens and sends ioctls to.
	accessDevice = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	// accessRunDir is the run directory: everything but running a file and
	// making a device node.
	accessRunDir = accessRead | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER
)

// fsRule grants access beneath one path.
type fsRule struct {
	path   string
	access uint64
}

// rulesetAttr is the ABI 6 form of struct landlock_ruleset_attr.
type rulesetAttr struct {
	handledFS  uint64
	handledNet uint64
	scoped     uint64
}

// landlockABI returns the ABI version the kernel reports.
func landlockABI() (int, error) {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, fmt.Errorf("read the Landlock ABI: %w", errno)
	}
	return int(v), nil
}

// enforceLandlock restricts the calling thread, and what it makes after, to
// the rules: every file system right is handled, so one no rule grants is
// denied; TCP bind and connect are handled with no rule, so neither is allowed;
// and the domain is scoped for abstract Unix sockets and signals. It needs
// no_new_privs, and fails on a kernel below ABI 6 rather than doing less.
func enforceLandlock(rules []fsRule) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	if abi < landlockABIRequired {
		return fmt.Errorf("the kernel reports Landlock ABI %d, and the confinement needs %d", abi, landlockABIRequired)
	}
	attr := rulesetAttr{
		handledFS:  handledFS,
		handledNet: unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP,
		scoped:     unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL,
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create the Landlock ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer unix.Close(ruleset)

	for _, r := range rules {
		if err := addRule(ruleset, r); err != nil {
			return err
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("enforce the Landlock ruleset: %w", errno)
	}
	return nil
}

// addRule grants the rule's rights beneath its path, which it opens as a
// descriptor only.
func addRule(ruleset int, r fsRule) error {
	fd, err := unix.Open(r.path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s for a Landlock rule: %w", r.path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s for a Landlock rule: %w", r.path, err)
	}
	access := r.access
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= fileRights
	}
	beneath := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&beneath)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("add the Landlock rule for %s: %w", r.path, errno)
	}
	return nil
}
