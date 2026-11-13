// Command osinit is the init of the OS-form guest the Driver tests boot: it
// writes what the guest kernel was given to the console, and waits.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

func main() {
	for _, m := range [][2]string{{"proc", "/proc"}, {"sysfs", "/sys"}} {
		if err := syscall.Mount(m[0], m[1], m[0], 0, ""); err != nil {
			fmt.Printf("os-form-guest: mount %s: %v\n", m[1], err)
		}
	}
	cmdline, _ := os.ReadFile("/proc/cmdline")
	fmt.Printf("os-form-guest: cmdline=%s\n", strings.TrimSpace(string(cmdline)))
	blocks, _ := filepath.Glob("/sys/block/vd*")
	sort.Strings(blocks)
	for _, b := range blocks {
		ro, _ := os.ReadFile(b + "/ro")
		fmt.Printf("os-form-guest: disk %s ro=%s\n", filepath.Base(b), strings.TrimSpace(string(ro)))
	}
	fmt.Println("os-form-guest: up")
	select {}
}
