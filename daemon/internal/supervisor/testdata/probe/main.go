// Command probe is the stand-in VMM of the confinement tests. It is a static
// executable, since the root it runs in holds no libraries, and it reports what
// the process it runs as can and cannot do.
//
//	probe vmm <dir>               a VMM that forks a daemonizing child, then runs on
//	probe report <dir> <try>...   write <dir>/report.json, then run on
//	probe check <dir> <try>...    write <dir>/report.json and exit
//	probe pids <dir> <n>          start up to n processes, write <dir>/pids.json, run on
//
// A try is one of: open:PATH, opendir:PATH, openw:PATH (open for writing, never
// creating), write:PATH, sha256:PATH (the SHA-256 in hex of what PATH holds),
// ioctl:PATH, abstract:NAME, unshare, setns, clone3, userns-clone, fork, fd3,
// ifaces.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "vmm":
		vmm(args[0])
	case "daemonize":
		daemonize(args[0])
	case "sleeper", "hold":
		sleep()
	case "noop":
	case "report", "check":
		report(args[0], args[1:])
		if os.Args[1] == "report" {
			sleep()
		}
	case "pids":
		n, _ := strconv.Atoi(args[1])
		pids(args[0], n)
		sleep()
	default:
		os.Exit(2)
	}
}

func sleep() { time.Sleep(24 * time.Hour) }

// vmm writes its pid, starts a child that daemonizes and exits, and runs on.
func vmm(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "vmm.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	_, _ = os.Stdout.WriteString("vmm started\n")
	pid, err := spawn(&syscall.SysProcAttr{Setsid: true}, nil, "daemonize", dir)
	if err != nil {
		os.Exit(1)
	}
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, 0, nil)
	sleep()
}

// daemonize starts a process in its own session and exits, so that the process
// is reparented to the namespace's init.
func daemonize(dir string) {
	if _, err := spawn(&syscall.SysProcAttr{Setsid: true}, nil, "sleeper", dir); err != nil {
		os.Exit(1)
	}
	_ = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600)
}

// spawn starts this executable by the path it was started with, since the root
// has no /proc/self/exe, on the descriptors 0 to 2, since it has no /dev/null
// for os/exec to open.
func spawn(attr *syscall.SysProcAttr, env []string, args ...string) (int, error) {
	return syscall.ForkExec(os.Args[0], append([]string{os.Args[0]}, args...),
		&syscall.ProcAttr{Files: []uintptr{0, 1, 2}, Env: env, Sys: attr})
}

// spawnAndWait runs this executable to its end.
func spawnAndWait(attr *syscall.SysProcAttr, args ...string) error {
	pid, err := spawn(attr, nil, args...)
	if err != nil {
		return err
	}
	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	return err
}

func report(dir string, tries []string) {
	out := map[string]string{}
	u, e, s := unix.Getresuid()
	g, eg, sg := unix.Getresgid()
	groups, _ := unix.Getgroups()
	out["uids"] = fmt.Sprint(u, e, s)
	out["gids"] = fmt.Sprint(g, eg, sg)
	out["groups"] = fmt.Sprint(len(groups))

	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		out["caps"] = "capget: " + err.Error()
	} else {
		out["caps.effective"] = fmt.Sprintf("%x:%x", data[1].Effective, data[0].Effective)
		out["caps.permitted"] = fmt.Sprintf("%x:%x", data[1].Permitted, data[0].Permitted)
		out["caps.inheritable"] = fmt.Sprintf("%x:%x", data[1].Inheritable, data[0].Inheritable)
	}
	bound, ambient := 0, 0
	for c := 0; c < 64; c++ {
		if v, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, uintptr(c), 0, 0, 0); err == nil && v == 1 {
			bound++
		}
		if v, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, uintptr(c), 0, 0); err == nil && v == 1 {
			ambient++
		}
	}
	out["caps.bounding"] = strconv.Itoa(bound)
	out["caps.ambient"] = strconv.Itoa(ambient)
	nnp, _ := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	out["nnp"] = strconv.Itoa(nnp)

	for _, try := range tries {
		kind, arg, _ := strings.Cut(try, ":")
		out[try] = attempt(kind, arg)
	}
	raw, _ := json.Marshal(out)
	tmp := filepath.Join(dir, ".report.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		os.Exit(1)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "report.json")); err != nil {
		os.Exit(1)
	}
}

// attempt does one try and returns "ok" or the name of the error.
func attempt(kind, arg string) string {
	switch kind {
	case "open":
		return errName(closing(unix.Open(arg, unix.O_RDONLY, 0)))
	case "opendir":
		return errName(closing(unix.Open(arg, unix.O_RDONLY|unix.O_DIRECTORY, 0)))
	case "openw":
		return errName(closing(unix.Open(arg, unix.O_WRONLY, 0)))
	case "sha256":
		raw, err := os.ReadFile(arg)
		if err != nil {
			return errName(unwrap(err))
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	case "write":
		return errName(closing(unix.Open(arg, unix.O_WRONLY|unix.O_CREAT, 0o600)))
	case "ioctl":
		fd, err := unix.Open(arg, unix.O_RDWR, 0)
		if err != nil {
			return "open " + errName(err)
		}
		defer unix.Close(fd)
		_, err = unix.IoctlGetTermios(fd, unix.TCGETS)
		if err == unix.ENOTTY {
			return "ok"
		}
		return errName(err)
	case "abstract":
		c, err := net.Dial("unix", "@"+arg)
		if err == nil {
			c.Close()
		}
		return errName(unwrap(err))
	case "unshare":
		return errName(unix.Unshare(0))
	case "setns":
		return errName(unix.Setns(-1, 0))
	case "clone3":
		_, _, errno := unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
		return errName(errno)
	case "userns-clone":
		return errName(spawnAndWait(&syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}, "noop"))
	case "fork":
		return errName(spawnAndWait(nil, "noop"))
	case "fd3":
		var ifr [40]byte
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, 3, 0x800454d2, uintptr(unsafe.Pointer(&ifr[0])))
		if errno != 0 {
			return errName(errno)
		}
		return "ok:" + strings.TrimRight(string(ifr[:16]), "\x00")
	case "ifaces":
		ifs, err := net.Interfaces()
		if err != nil {
			return err.Error()
		}
		var names []string
		for _, i := range ifs {
			names = append(names, i.Name)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	return "unknown try"
}

func closing(fd int, err error) error {
	if err == nil {
		unix.Close(fd)
	}
	return err
}

func errName(err error) string {
	if err == nil {
		return "ok"
	}
	if errno, ok := err.(syscall.Errno); ok {
		if errno == 0 {
			return "ok"
		}
		if n := unix.ErrnoName(errno); n != "" {
			return n
		}
	}
	return err.Error()
}

// unwrap returns the errno inside an error from the os or net packages.
func unwrap(err error) error {
	for err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			return errno
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
	return nil
}

// pids starts up to n processes that hold, with one OS thread each where the
// Go runtime allows, and records how many started and why the next did not.
func pids(dir string, n int) {
	started, reason := 0, "none"
	for i := 0; i < n; i++ {
		if _, err := spawn(nil, []string{"GOMAXPROCS=1"}, "hold"); err != nil {
			reason = errName(err)
			break
		}
		started++
	}
	raw, _ := json.Marshal(map[string]any{"started": started, "stopped_by": reason})
	tmp := filepath.Join(dir, ".pids.tmp")
	_ = os.WriteFile(tmp, raw, 0o600)
	_ = os.Rename(tmp, filepath.Join(dir, "pids.json"))
}
