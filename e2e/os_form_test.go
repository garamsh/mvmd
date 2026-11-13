//go:build e2e_real

package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const osFormSet = "os-test"

// An OS-form Machine created from a boot set the node offers runs its whole life: it boots its
// own kernel and initrd with no image and no Guest Agent, its init reads the read-only disk and is
// refused a write to it, a token it keeps on the read-write disk is still there when the Machine
// is stopped and started again, and the Machine is stopped and deleted. The guest answers on the
// console, which is the only witness there is.
func TestE2E_AnOSFormMachineBootsItsBootSetKeepsItsDataDiskAndIsStoppedAndDeleted(t *testing.T) {
	const diskBytes = 32 << 20
	token := randomToken(t)
	n := startNodeWith(t, withOSFormBootSet(osFormSet))
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, info.GetBootSets(), 1)
	assert.Equal(t, osFormSet, info.BootSets[0].Name, "the node offers the boot set")
	assert.Equal(t, os.Getenv(envKernelSHA), info.BootSets[0].KernelDigest, "the boot set's kernel is the pinned one")
	assert.NotEmpty(t, info.BootSets[0].InitrdDigest)

	// The read-only disk is an ext4 one, whose superblock the guest reads; the read-write disk has
	// no filesystem and is empty.
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "os-ro", RequestId: "create-os-ro", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "os-rw", RequestId: "create-os-rw", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	created, err := n.client.CreateMachine(ctx, &v1alpha2.CreateMachineRequest{
		MachineId: "os",
		RequestId: "create-os",
		Resources: &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
		OsBoot:    &v1alpha2.OsBoot{BootSet: osFormSet, KernelCmdline: "mvmd.token=" + token},
		Disks:     []*v1alpha2.DiskAttachment{{DiskId: "os-ro", ReadOnly: true}, {DiskId: "os-rw"}},
	})
	require.NoError(t, err)
	assert.Equal(t, osFormSet, created.Machine.GetOsBoot().GetBootSet(), "the Machine is the OS form of the boot set")
	require.Len(t, created.Machine.Disks, 2)
	assert.True(t, created.Machine.Disks[0].ReadOnly, "the first disk is attached read-only")
	assert.False(t, created.Machine.Disks[1].ReadOnly, "the second disk is attached read-write")

	start := func() osFormReport {
		started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "os"})
		require.NoError(t, err)
		require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
		return n.osFormReport(t, ctx, "os")
	}
	first := start()
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: "empty", Written: token}, first,
		"the first boot: the guest's command line holds the token, its first disk is read-only and holds an ext4 superblock, a write to it is refused, and the second disk was empty and took the token")

	stopped, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "os", GracePeriod: durationpb.New(3 * time.Second)})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stopped.Machine.State)
	n.waitVMMGone(t, "os")

	second := start()
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: token, Written: "none"}, second,
		"the second boot finds the token the first wrote, and writes none")

	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "os", GracePeriod: durationpb.New(3 * time.Second)})
	require.NoError(t, err)
	n.waitVMMGone(t, "os")
	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "os"})
	require.NoError(t, err)

	assert.NotContains(t, n.listed(t, ctx), "os", "the Machine is gone")
	assert.Equal(t, map[string]string{"os-ro": "", "os-rw": ""}, n.holders(t, ctx), "its disks are given back to the node")
}

// osFormReport is the line the OS-form guest's init writes to the console.
type osFormReport struct {
	Token, ReadOnly0, ReadOnly1, Magic, ROWrite, Before, Written string
}

var osFormLine = regexp.MustCompile(`os-form-e2e: token=(\S*) ro0=(\S+) ro1=(\S+) magic=(\S+) ro-write=(\S+) data-before=(\S+) data-written=(\S+) end\r?\n`)

// osFormReport opens a Console session and returns the first whole line the guest's init writes.
// A session carries only what is written after it opened, so the init repeats its line.
func (n *node) osFormReport(t *testing.T, ctx context.Context, machine string) osFormReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stream, err := n.client.Console(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&v1alpha2.ConsoleRequest{MachineId: machine}))
	var seen bytes.Buffer
	for {
		part, err := stream.Recv()
		require.NoError(t, err, "the console of %s gave no line of the init; the output so far: %q", machine, seen.String())
		seen.Write(part.GetOutput())
		if m := osFormLine.FindSubmatch(seen.Bytes()); m != nil {
			return osFormReport{string(m[1]), string(m[2]), string(m[3]), string(m[4]), string(m[5]), string(m[6]), string(m[7])}
		}
	}
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// withOSFormBootSet offers a boot set of the pinned kernel and an initrd built here: the init of
// ./osinit in a newc archive, pinned by its digest.
func withOSFormBootSet(name string) nodeOption {
	return func(n *node, config map[string]any) {
		inputs := filepath.Join(n.work, "inputs")
		kernel := copyFile(n.t, os.Getenv(envKernel), filepath.Join(inputs, "os-vmlinux"), 0o644)
		initrd := filepath.Join(inputs, "os-initrd.cpio")
		archive := osFormInitrd(n.t)
		require.NoError(n.t, os.WriteFile(initrd, archive, 0o644))
		sum := sha256.Sum256(archive)
		config[driver.section].(map[string]any)["boot_sets"] = map[string]any{
			name: map[string]any{
				"kernel": map[string]any{"path": kernel, "sha256": os.Getenv(envKernelSHA)},
				"initrd": map[string]any{"path": initrd, "sha256": hex.EncodeToString(sum[:])},
			},
		}
	}
}

// osFormInitrd builds ./osinit as a static binary and returns a newc archive that holds it as
// /init, with the directories it mounts over and the console device the kernel opens for it.
func osFormInitrd(t *testing.T) []byte {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "osinit")
	build := exec.Command("go", "build", "-o", bin, "./osinit")
	// A cache of its own: the tests run as root, and root must not fill the user's.
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOTOOLCHAIN=local", "GOCACHE="+t.TempDir())
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	guestInit, err := os.ReadFile(bin)
	require.NoError(t, err)

	var archive bytes.Buffer
	for _, e := range []cpioEntry{
		{name: "dev", mode: 0o040755},
		{name: "dev/console", mode: 0o020600, rdevMajor: 5, rdevMinor: 1},
		{name: "proc", mode: 0o040755},
		{name: "sys", mode: 0o040755},
		{name: "init", mode: 0o100755, data: guestInit},
		{name: "TRAILER!!!"},
	} {
		e.writeTo(&archive)
	}
	return archive.Bytes()
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
	padToFour(w)
	w.Write(e.data)
	padToFour(w)
}

func padToFour(w *bytes.Buffer) {
	for w.Len()%4 != 0 {
		w.WriteByte(0)
	}
}
