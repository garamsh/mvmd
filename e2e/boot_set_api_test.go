//go:build e2e_real

package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const addedSet = "os-added"

// bootSetChunk is the size of the messages a boot set is streamed in, under the 4 MiB a message may
// be.
const bootSetChunk = 1 << 20

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// addBootSet streams a boot set to the node: the message that names it and carries the digests it
// declares, then the kernel's chunks, then the initrd's.
func (n *node) addBootSet(ctx context.Context, name, kernelDigest, initrdDigest string, kernel, initrd []byte) (*v1alpha2.AddBootSetResponse, error) {
	n.t.Helper()
	stream, err := n.client.AddBootSet(ctx)
	require.NoError(n.t, err)
	send := func(m *v1alpha2.AddBootSetRequest) {
		n.t.Helper()
		// A send fails with io.EOF once the node has ended the call; the call's own outcome is
		// what CloseAndRecv returns.
		_ = stream.Send(m)
	}
	send(&v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_Start{Start: &v1alpha2.AddBootSetStart{
		Name: name, KernelDigest: kernelDigest, InitrdDigest: initrdDigest,
	}}})
	for rest := kernel; len(rest) > 0; {
		k := min(bootSetChunk, len(rest))
		send(&v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_KernelChunk{KernelChunk: rest[:k]}})
		rest = rest[k:]
	}
	for rest := initrd; len(rest) > 0; {
		k := min(bootSetChunk, len(rest))
		send(&v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_InitrdChunk{InitrdChunk: rest[:k]}})
		rest = rest[k:]
	}
	return stream.CloseAndRecv()
}

// bootSets are the node's boot sets by name.
func (n *node) bootSets(ctx context.Context) map[string]*v1alpha2.BootSet {
	n.t.Helper()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(n.t, err)
	sets := map[string]*v1alpha2.BootSet{}
	for _, s := range info.GetBootSets() {
		sets[s.GetName()] = s
	}
	return sets
}

// A client adds a boot set to a running node by streaming its two files, an OS-form Machine is
// created from it and boots on it, the set is listed with its digests and origin beside a
// configured one and is still there when mvmd restarts, a set a Machine names is not removed and
// one no Machine names is, and a configured set is never removed through the API. A set whose
// initrd is not its declared digest is refused and not published.
func TestE2E_AClientAddsABootSetAMachineBootsOnItAndTheSetIsRemovedOnlyWhenNoMachineNamesIt(t *testing.T) {
	const diskBytes = 32 << 20
	token := randomToken(t)
	n := startNodeWith(t, withOSFormBootSet(osFormSet))
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	kernel, err := os.ReadFile(os.Getenv(envKernel))
	require.NoError(t, err)
	initrd := osFormInitrd(t)
	kernelDigest, initrdDigest := hexDigest(kernel), hexDigest(initrd)
	require.Equal(t, os.Getenv(envKernelSHA), kernelDigest, "the setup: the kernel read here is the pinned one")

	_, err = n.addBootSet(ctx, addedSet, kernelDigest, hexDigest([]byte("not the initrd")), kernel, initrd)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "%v", err)
	assert.ErrorContains(t, err, "initrd", "the refusal names the file")
	assert.Len(t, n.bootSets(ctx), 1, "nothing was published: only the configured set is listed")

	added, err := n.addBootSet(ctx, addedSet, kernelDigest, initrdDigest, kernel, initrd)
	require.NoError(t, err, "control: the same files with their own digests are added")
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED, added.GetBootSet().GetOrigin())

	sets := n.bootSets(ctx)
	require.Len(t, sets, 2)
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_CONFIGURED, sets[osFormSet].GetOrigin())
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED, sets[addedSet].GetOrigin())
	assert.Equal(t, kernelDigest, sets[addedSet].GetKernelDigest())
	assert.Equal(t, initrdDigest, sets[addedSet].GetInitrdDigest())

	_, err = n.addBootSet(ctx, addedSet, kernelDigest, initrdDigest, kernel, initrd)
	require.NoError(t, err, "the same name with the same digests is success")
	_, err = n.addBootSet(ctx, addedSet, kernelDigest, hexDigest([]byte("another")), kernel, []byte("another"))
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, v1alpha2.KindOf(err), "the same name with other digests: %v", err)

	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "added-ro", RequestId: "create-added-ro", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "added-rw", RequestId: "create-added-rw", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	created, err := n.client.CreateMachine(ctx, &v1alpha2.CreateMachineRequest{
		MachineId: "added",
		RequestId: "create-added",
		Resources: &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
		OsBoot:    &v1alpha2.OsBoot{BootSet: addedSet, KernelCmdline: "mvmd.token=" + token},
		Disks:     []*v1alpha2.DiskAttachment{{DiskId: "added-ro", ReadOnly: true}, {DiskId: "added-rw"}},
	})
	require.NoError(t, err)
	assert.Equal(t, addedSet, created.Machine.GetOsBoot().GetBootSet())

	start := func() osFormReport {
		started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "added"})
		require.NoError(t, err)
		require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
		return n.osFormReport(t, ctx, "added")
	}
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: "empty", Written: token}, start(),
		"the Machine booted its own init from the added set: the VMM ran the kernel and initrd the client sent")

	_, err = n.client.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: addedSet})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a set a Machine names: %v", err)
	_, err = n.client.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: osFormSet})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a configured set: %v", err)

	n.restartDaemon()
	sets = n.bootSets(ctx)
	require.Contains(t, sets, addedSet, "the added set survived the restart")
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED, sets[addedSet].GetOrigin())
	assert.Equal(t, kernelDigest, sets[addedSet].GetKernelDigest())

	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "added", GracePeriod: durationpb.New(3 * time.Second)})
	require.NoError(t, err)
	n.waitVMMGone(t, "added")
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: token, Written: "none"}, start(),
		"after the restart the Machine boots on the set again, and finds the token the first boot wrote")

	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "added", GracePeriod: durationpb.New(3 * time.Second)})
	require.NoError(t, err)
	n.waitVMMGone(t, "added")
	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "added"})
	require.NoError(t, err)

	_, err = n.client.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: addedSet})
	require.NoError(t, err, "no Machine names the set now")
	sets = n.bootSets(ctx)
	assert.Len(t, sets, 1, "only the configured set is left")
	assert.Contains(t, sets, osFormSet)
}
