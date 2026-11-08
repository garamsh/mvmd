package supervisor

import (
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type landlockArgs struct {
	Granted, Denied, Run, TCP, Bind, Abstract string
	Parent                                    int
}

func init() {
	cases["landlock"] = func(raw []byte) (map[string]string, error) {
		var a landlockArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		res := map[string]string{}
		try := func(phase string) {
			open := func(path string, flags int) string {
				fd, err := unix.Open(path, flags, 0o600)
				if err == nil {
					unix.Close(fd)
				}
				return errName(err)
			}
			ioctl := func(path string) string {
				fd, err := unix.Open(path, unix.O_RDWR, 0)
				if err != nil {
					return "open: " + errName(err)
				}
				defer unix.Close(fd)
				// /dev/null and /dev/zero are not terminals: reaching the
				// driver answers ENOTTY, which is the success here.
				if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == unix.ENOTTY {
					return "ok"
				} else {
					return errName(err)
				}
			}
			res[phase+" read granted"] = open(a.Granted+"/f", unix.O_RDONLY)
			res[phase+" read denied"] = open(a.Denied+"/f", unix.O_RDONLY)
			res[phase+" list denied"] = open(a.Denied, unix.O_RDONLY|unix.O_DIRECTORY)
			res[phase+" write run"] = open(a.Run+"/"+strings.ReplaceAll(phase, " ", "-"), unix.O_WRONLY|unix.O_CREAT)
			res[phase+" write granted read-only"] = open(a.Granted+"/f", unix.O_WRONLY)
			res[phase+" ioctl on a device with the right"] = ioctl("/dev/zero")
			res[phase+" ioctl on a device without it"] = ioctl("/dev/null")
			c, err := net.Dial("tcp", a.TCP)
			if err == nil {
				c.Close()
			}
			res[phase+" tcp connect"] = errName(unwrapNet(err))
			res[phase+" tcp bind"] = errName(bindTCP(a.Bind))
			u, err := net.Dial("unix", "@"+a.Abstract)
			if err == nil {
				u.Close()
			}
			res[phase+" abstract unix socket"] = errName(unwrapNet(err))
			res[phase+" signal to a process outside"] = errName(unix.Kill(a.Parent, 0))
		}
		try("before")
		if err := noNewPrivs(); err != nil {
			return nil, err
		}
		if err := enforceLandlock([]fsRule{
			{a.Granted, accessRead},
			{a.Run, accessRunDir},
			{"/dev/zero", accessDevice},
			{"/dev/null", accessDevice &^ unix.LANDLOCK_ACCESS_FS_IOCTL_DEV},
		}); err != nil {
			return nil, err
		}
		try("after")
		// A process inside the domain, the child itself, is still signalled.
		res["after signal to a process inside"] = errName(unix.Kill(os.Getpid(), 0))
		return res, nil
	}
}

// bindTCP binds a TCP socket to the address, through the system calls
// themselves, so that the thread that makes the call is the one this case
// restricted.
func bindTCP(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Bind(fd, &unix.SockaddrInet4{Port: int(ap.Port()), Addr: ap.Addr().As4()})
}

func unwrapNet(err error) error {
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

func TestLandlockABI_TheHostOffersTheVersionTheConfinementNeeds(t *testing.T) {
	abi, err := landlockABI()
	require.NoError(t, err, "the host has no Landlock")
	t.Logf("host: Landlock ABI %d", abi)
	assert.GreaterOrEqual(t, abi, 6)
}

// What the enforced ruleset takes away, each beside the same try before it,
// which shows the try succeeds without the ruleset.
func TestEnforceLandlock_TakesAwayWhatNoRuleGrants(t *testing.T) {
	dir := t.TempDir()
	granted, denied, run := filepath.Join(dir, "granted"), filepath.Join(dir, "denied"), filepath.Join(dir, "run")
	for _, d := range []string{granted, denied, run} {
		require.NoError(t, os.Mkdir(d, 0o755))
		require.NoError(t, os.WriteFile(d+"/f", []byte("x"), 0o644))
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { tcp.Close() })
	abstract, err := net.Listen("unix", "@mvmd-landlock-test-"+filepath.Base(dir))
	require.NoError(t, err)
	t.Cleanup(func() { abstract.Close() })
	free, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	bind := free.Addr().String()
	require.NoError(t, free.Close(), "the port the child binds is free")
	go acceptAll(tcp)
	go acceptAll(abstract)

	res, err := child(t, "landlock", landlockArgs{
		Granted: granted, Denied: denied, Run: run, TCP: tcp.Addr().String(), Bind: bind,
		Abstract: "mvmd-landlock-test-" + filepath.Base(dir), Parent: os.Getpid(),
	})
	require.NoError(t, err)

	for _, c := range []struct{ try, before, after string }{
		{"read granted", "ok", "ok"},
		{"read denied", "ok", "EACCES"},
		{"list denied", "ok", "EACCES"},
		{"write run", "ok", "ok"},
		{"write granted read-only", "ok", "EACCES"},
		{"ioctl on a device with the right", "ok", "ok"},
		{"ioctl on a device without it", "ok", "EACCES"},
		{"tcp connect", "ok", "EACCES"},
		{"tcp bind", "ok", "EACCES"},
		{"abstract unix socket", "ok", "EPERM"},
		{"signal to a process outside", "ok", "EPERM"},
	} {
		assert.Equal(t, c.before, res["before "+c.try], "control, before the ruleset: %s", c.try)
		assert.Equal(t, c.after, res["after "+c.try], "after the ruleset: %s", c.try)
	}
	assert.Equal(t, "ok", res["after signal to a process inside"], "a process of the domain is still signalled")
}

func acceptAll(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

func TestRequireLandlockABI_RefusesAKernelBelowSix(t *testing.T) {
	assert.Error(t, RequireLandlockABI(5))
	assert.Error(t, RequireLandlockABI(0))
	assert.NoError(t, RequireLandlockABI(6), "control: the ABI the confinement needs is accepted")
	assert.NoError(t, RequireLandlockABI(8), "control: a later ABI is accepted")
}
