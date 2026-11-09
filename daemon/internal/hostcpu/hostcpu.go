// Package hostcpu names the model of the host's CPU: a snapshot restores only
// on the same model (driver.md).
package hostcpu

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// fields are the /proc/cpuinfo keys that identify a CPU model, by
// architecture.
var fields = []string{
	// x86_64
	"vendor_id", "cpu family", "model", "stepping",
	// aarch64
	"CPU implementer", "CPU architecture", "CPU variant", "CPU part", "CPU revision",
}

// Model names the model of the host's first processor.
func Model() (string, error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("read the CPU model: %w", err)
	}
	defer f.Close()
	return ModelOf(f)
}

// ModelOf names the model of the first processor in a /proc/cpuinfo.
func ModelOf(r io.Reader) (string, error) {
	var parts []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" && len(parts) > 0 {
			break
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		for _, f := range fields {
			if key == f {
				parts = append(parts, key+"="+val)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read the CPU model: %w", err)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("the CPU description names none of %q", fields)
	}
	return strings.Join(parts, " "), nil
}
