package supervisor

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// The tun driver's requests (include/uapi/linux/if_tun.h).
const tunSetIff = 0x400454ca

// openTap attaches to the persistent tap of the given name in the calling
// network namespace and returns its descriptor. The tap must exist: opening
// /dev/net/tun with a name that no tap has would make a transient one.
func openTap(name string) (*os.File, error) {
	if _, err := net.InterfaceByName(name); err != nil {
		return nil, fmt.Errorf("the tap %q does not exist: %w", name, err)
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if err := unix.IoctlIfreq(fd, tunSetIff, ifr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("attach to the tap %q: %w", name, err)
	}
	return os.NewFile(uintptr(fd), "tap "+name), nil
}
