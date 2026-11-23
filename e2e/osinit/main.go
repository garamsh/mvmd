// Command osinit is the init of the OS-form guest the end-to-end tests boot. It reads the magic of
// the read-only disk's ext4 superblock, tries to write that disk, keeps a token from its command
// line on the read-write disk, and writes what it found to the console as one line, again every
// half second. Given mvmd.seed=N on its command line, it also reads the first N bytes of a third,
// read-only disk and writes them in hex on a line of their own, as a guest reads a NoCloud seed.
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
	// seedDisk is the third disk, which holds the seed, and seedField names how many of its first
	// bytes the init reads.
	seedDisk  = "vdc"
	seedField = "mvmd.seed="
	diskWait  = 30 * time.Second
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
	seedLine := ""
	if n := commandLineField(seedField); n != "" {
		seedLine = seed(device(seedDisk), n)
	}
	for {
		fmt.Println(line)
		if seedLine != "" {
			fmt.Println(seedLine)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// commandLineToken is the value of mvmd.token on the kernel's command line, or "".
func commandLineToken() string { return commandLineField(tokenField) }

// commandLineField is the value of the field, which ends in "=", on the kernel's command line, or "".
func commandLineField(name string) string {
	raw, _ := os.ReadFile("/proc/cmdline")
	for field := range strings.FieldsSeq(string(raw)) {
		if v, ok := strings.CutPrefix(field, name); ok {
			return v
		}
	}
	return ""
}

// seed is the line that reports whether the disk at path is read-only and its first n bytes, read
// at offset 0, in hex, or the reason they were not read.
func seed(path, n string) string {
	var size int
	if _, err := fmt.Sscanf(n, "%d", &size); err != nil || size <= 0 {
		return "os-form-e2e-seed: ro=bad-size bytes=none end"
	}
	read := "open-failed"
	if f, err := os.Open(path); err == nil {
		b := make([]byte, size)
		if _, err := f.ReadAt(b, 0); err != nil {
			read = "read-failed"
		} else {
			read = fmt.Sprintf("%x", b)
		}
		f.Close()
	}
	return "os-form-e2e-seed: ro=" + sysfs(seedDisk, "ro") + " bytes=" + read + " end"
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
