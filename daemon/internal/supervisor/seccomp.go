package supervisor

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The seccomp_data offsets a filter reads (include/uapi/linux/seccomp.h).
const (
	offsetNR   = 0
	offsetArch = 4
	offsetArg0 = 16
)

// x32Bit marks an x86-64 system call number as an x32 one, which the kernel
// numbers apart from the amd64 calls the filter names.
const x32Bit = 0x40000000

// namespaceFlags are the clone flags that create a namespace.
const namespaceFlags = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS |
	unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// auditArch is the architecture number the filter admits.
func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xC000003E, nil
	case "arm64":
		return 0xC00000B7, nil
	}
	return 0, fmt.Errorf("the seccomp baseline is not written for %s", runtime.GOARCH)
}

// baselineFilter returns the baseline of ADR 0015 as a classic BPF program:
// EPERM for unshare, setns and clone with a namespace flag, ENOSYS for clone3,
// and the process ended for a call of another architecture or through the x32
// numbers, which would reach those calls unfiltered. Everything else is
// allowed.
func baselineFilter() ([]unix.SockFilter, error) {
	arch, err := auditArch()
	if err != nil {
		return nil, err
	}
	const (
		ld  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jst = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret = unix.BPF_RET | unix.BPF_K

		allow = 0x7fff0000                       // SECCOMP_RET_ALLOW
		kill  = 0x80000000                       // SECCOMP_RET_KILL_PROCESS
		eperm = 0x00050000 | uint32(unix.EPERM)  // SECCOMP_RET_ERRNO | EPERM
		nosys = 0x00050000 | uint32(unix.ENOSYS) // SECCOMP_RET_ERRNO | ENOSYS
	)
	// The program is laid out as: the tests, then the four verdicts. A test
	// names the verdict it jumps to; offsets are resolved after the layout.
	type test struct {
		code    uint16
		k       uint32
		onTrue  string // verdict or "next"
		onFalse string
	}
	tests := []test{
		{ld, offsetArch, "", ""},
		{jeq, arch, "next", "kill"},
		{ld, offsetNR, "", ""},
		{jst, x32Bit, "kill", "next"},
		{jeq, unix.SYS_UNSHARE, "eperm", "next"},
		{jeq, unix.SYS_SETNS, "eperm", "next"},
		{jeq, unix.SYS_CLONE3, "nosys", "next"},
		{jeq, unix.SYS_CLONE, "next", "allow"},
		{ld, offsetArg0, "", ""},
		{jst, namespaceFlags, "eperm", "allow"},
	}
	verdicts := []struct {
		name string
		k    uint32
	}{{"allow", allow}, {"kill", kill}, {"eperm", eperm}, {"nosys", nosys}}

	at := map[string]int{}
	for i, v := range verdicts {
		at[v.name] = len(tests) + i
	}
	var prog []unix.SockFilter
	for i, t := range tests {
		ins := unix.SockFilter{Code: t.code, K: t.k}
		if t.code != ld {
			jump := func(to string) uint8 {
				target := i + 1
				if to != "next" {
					target = at[to]
				}
				return uint8(target - (i + 1))
			}
			ins.Jt, ins.Jf = jump(t.onTrue), jump(t.onFalse)
		}
		prog = append(prog, ins)
	}
	for _, v := range verdicts {
		prog = append(prog, unix.SockFilter{Code: ret, K: v.k})
	}
	return prog, nil
}

// installBaseline installs the baseline on the calling thread, which the
// children it makes and the program it execs inherit. It needs no_new_privs.
func installBaseline() error {
	filter, err := baselineFilter()
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	const secompSetModeFilter = 1
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, secompSetModeFilter, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("install the seccomp baseline: %w", errno)
	}
	return nil
}
