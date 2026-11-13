package drivertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
)

// AnOSFormGuestBootsItsBootSetAndTellsTheConsoleWhatTheKernelWasGiven boots an
// OS-form Machine: the pinned kernel and an initrd built here whose init writes
// the kernel's command line and its block devices to the console, with two
// disks, the first read-only. No Guest Agent runs, so the console is the only
// witness.
func AnOSFormGuestBootsItsBootSetAndTellsTheConsoleWhatTheKernelWasGiven(t *testing.T, s Subject) {
	state := longTempDir(t, "dtos")
	run := statedir.Run(state)
	require.NoError(t, os.Mkdir(run, 0o700))
	d := s.Driver(t, RunDirs{"m1": run})

	initrd := osInitrd(t, state)
	spec := machine.BootSpec{
		Kernel:      s.Kernel,
		Initrd:      initrd,
		CommandLine: "mvmd.test=osform",
		Devices:     []machine.BlockDevice{diskFile(t, state, "data", true), diskFile(t, state, "scratch", false)},
		Resources:   resources,
		RunDir:      run,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	assert.Contains(t, p.BootFiles, machine.File{Host: initrd.Path, In: machine.InInitrd, ReadOnly: true, SHA256: initrd.SHA256},
		"the description boots the boot set's initrd")

	startLogged(t, s, p.For(spec), run)
	require.NoError(t, d.Boot(ctx, "m1", spec))

	var raw []byte
	require.Eventually(t, func() bool {
		log, err := d.ConsoleLog(ctx, "m1")
		if err != nil {
			return false
		}
		raw, _ = io.ReadAll(log)
		_ = log.Close()
		return bytes.Contains(raw, []byte("os-form-guest: up"))
	}, time.Minute, 500*time.Millisecond, "the OS-form guest never wrote its line to the console")

	i := bytes.Index(raw, []byte("os-form-guest: cmdline="))
	require.GreaterOrEqual(t, i, 0, "the guest wrote its command line")
	line, _, _ := bytes.Cut(raw[i:], []byte("\n"))
	assert.Regexp(t, `cmdline=console=\S+ reboot=k panic=1( .*)? mvmd\.test=osform\r?$`, string(line),
		"the client's line follows the Driver's parameters")
	assert.NotContains(t, string(line), "mvmd.end", "no Guest Agent reads it")
	first, second := bytes.Index(raw, []byte("os-form-guest: disk vda ro=1")), bytes.Index(raw, []byte("os-form-guest: disk vdb ro=0"))
	assert.GreaterOrEqual(t, first, 0, "the first disk is drive 0, read-only")
	assert.Greater(t, second, first, "the second disk follows, read-write")
}

// osInitrd builds the OS-form guest's initrd in dir: a static init from
// ./osinit in a newc archive, with the console device the kernel opens for it.
// It returns the file with its SHA-256, which is the digest the boot set pins.
func osInitrd(t *testing.T, dir string) machine.KernelFile {
	t.Helper()
	bin := filepath.Join(dir, "osinit")
	build := exec.Command("go", "build", "-o", bin, "github.com/garamsh/mvmd/daemon/internal/drivertest/osinit")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	guestInit, err := os.ReadFile(bin)
	require.NoError(t, err)

	var cpio bytes.Buffer
	for _, e := range []cpioEntry{
		{name: "dev", mode: 0o040755},
		{name: "dev/console", mode: 0o020600, rdevMajor: 5, rdevMinor: 1},
		{name: "proc", mode: 0o040755},
		{name: "sys", mode: 0o040755},
		{name: "init", mode: 0o100755, data: guestInit},
		{name: "TRAILER!!!"},
	} {
		e.writeTo(&cpio)
	}
	path := filepath.Join(dir, "os-initrd.cpio")
	require.NoError(t, os.WriteFile(path, cpio.Bytes(), 0o600))
	sum := sha256.Sum256(cpio.Bytes())
	return machine.KernelFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

// cpioEntry is one member of a newc archive.
type cpioEntry struct {
	name                 string
	mode                 uint32
	rdevMajor, rdevMinor uint32
	data                 []byte
}

func (e cpioEntry) writeTo(w *bytes.Buffer) {
	name := e.name + "\x00"
	fmt.Fprintf(w, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		0, e.mode, 0, 0, 1, 0, len(e.data), 0, 0, e.rdevMajor, e.rdevMinor, len(name), 0)
	w.WriteString(name)
	pad(w)
	w.Write(e.data)
	pad(w)
}

func pad(w *bytes.Buffer) {
	for w.Len()%4 != 0 {
		w.WriteByte(0)
	}
}
