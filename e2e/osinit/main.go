// Command osinit is the init of the OS-form guest the end-to-end tests boot. It reads the magic of
// the read-only disk's ext4 superblock, tries to write that disk, keeps a token from its command
// line on the read-write disk, and writes what it found to the console as one line, again every
// half second.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// The disks, in the order the Machine attaches them, and what the init reads of them.
const (
	readOnlyDisk  = "vda"
	readWriteDisk = "vdb"
	// magicOffset is where an ext4 superblock holds its magic, 0xEF53, little-endian.
	magicOffset = 1080
	tokenPrefix = "mvmd-e2e-token:"
	tokenField  = "mvmd.token="
	diskWait    = 30 * time.Second
)

func main() {
	for _, m := range [][2]string{{"proc", "/proc"}, {"sysfs", "/sys"}} {
		if err := syscall.Mount(m[0], m[1], m[0], 0, ""); err != nil {
			fmt.Printf("os-form-e2e: mount %s: %v\n", m[1], err)
		}
	}
	token := commandLineToken()
	readOnly, readWrite := device(readOnlyDisk), device(readWriteDisk)

	before, written := keepToken(readWrite, token)
	line := strings.Join([]string{
		"os-form-e2e:",
		"token=" + token,
		"ro0=" + sysfs(readOnlyDisk, "ro"),
		"ro1=" + sysfs(readWriteDisk, "ro"),
		"magic=" + magic(readOnly),
		"ro-write=" + tryWrite(readOnly),
		"data-before=" + before,
		"data-written=" + written,
		"end",
	}, " ")
	for {
		fmt.Println(line)
		time.Sleep(500 * time.Millisecond)
	}
}

// commandLineToken is the value of mvmd.token on the kernel's command line, or "".
func commandLineToken() string {
	raw, _ := os.ReadFile("/proc/cmdline")
	for _, field := range strings.Fields(string(raw)) {
		if v, ok := strings.CutPrefix(field, tokenField); ok {
			return v
		}
	}
	return ""
}

func sysfs(disk, name string) string {
	raw, err := os.ReadFile("/sys/block/" + disk + "/" + name)
	if err != nil {
		return "missing"
	}
	return strings.TrimSpace(string(raw))
}

// device makes the node of a disk, once the kernel has found it, and returns its path.
func device(disk string) string {
	path := "/dev/" + disk
	numbers := "missing"
	for deadline := time.Now().Add(diskWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if numbers = sysfs(disk, "dev"); numbers != "missing" {
			break
		}
	}
	var major, minor int
	if _, err := fmt.Sscanf(numbers, "%d:%d", &major, &minor); err != nil {
		return path
	}
	if err := syscall.Mknod(path, syscall.S_IFBLK|0o600, major<<8|minor); err != nil {
		fmt.Printf("os-form-e2e: mknod %s: %v\n", path, err)
	}
	return path
}

// magic is the two bytes of an ext4 superblock's magic in hex, or the reason they were not read.
func magic(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "open-failed"
	}
	defer f.Close()
	b := make([]byte, 2)
	if _, err := f.ReadAt(b, magicOffset); err != nil {
		return "read-failed"
	}
	return fmt.Sprintf("%02x%02x", b[0], b[1])
}

// tryWrite writes a byte at the start of a disk, which an ext4 filesystem does not use, and says
// whether the guest refused it.
func tryWrite(path string) string {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "refused"
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{0xff}, 0); err != nil {
		return "refused"
	}
	if err := f.Sync(); err != nil {
		return "refused"
	}
	return "accepted"
}

// keepToken returns the token the disk held at boot, "empty" if it held none, and the token it
// wrote there, "none" if it wrote none. A token is synced to the disk before it is reported.
func keepToken(path, token string) (before, written string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "open-failed", "none"
	}
	defer f.Close()
	b := make([]byte, 128)
	n, _ := f.ReadAt(b, 0)
	if held, ok := strings.CutPrefix(string(b[:n]), tokenPrefix); ok {
		held, _, _ = strings.Cut(held, "\n")
		return held, "none"
	}
	if _, err := f.WriteAt([]byte(tokenPrefix+token+"\n"), 0); err != nil {
		return "empty", "write-failed"
	}
	if err := f.Sync(); err != nil {
		return "empty", "sync-failed"
	}
	return "empty", token
}
