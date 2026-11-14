package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func startMessage(name string, kernel, initrd []byte) *v1alpha2.AddBootSetRequest {
	return &v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_Start{Start: &v1alpha2.AddBootSetStart{
		Name: name, KernelDigest: hexSum(kernel), InitrdDigest: hexSum(initrd),
	}}}
}

func kernelChunk(b []byte) *v1alpha2.AddBootSetRequest {
	return &v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_KernelChunk{KernelChunk: b}}
}

func initrdChunk(b []byte) *v1alpha2.AddBootSetRequest {
	return &v1alpha2.AddBootSetRequest{Part: &v1alpha2.AddBootSetRequest_InitrdChunk{InitrdChunk: b}}
}

// addBootSet streams the messages and returns the call's outcome.
func addBootSet(t *testing.T, client v1alpha2.RuntimeServiceClient, msgs ...*v1alpha2.AddBootSetRequest) (*v1alpha2.AddBootSetResponse, error) {
	t.Helper()
	stream, err := client.AddBootSet(context.Background())
	require.NoError(t, err)
	for _, m := range msgs {
		require.NoError(t, stream.Send(m))
	}
	return stream.CloseAndRecv()
}

func wholeSet(name string, kernel, initrd []byte) []*v1alpha2.AddBootSetRequest {
	return []*v1alpha2.AddBootSetRequest{
		startMessage(name, kernel, initrd), kernelChunk(kernel[:2]), kernelChunk(kernel[2:]), initrdChunk(initrd[:3]), initrdChunk(initrd[3:]),
	}
}

func TestAddBootSet_StreamsTheFilesAndListsTheSetWithItsOrigin(t *testing.T) {
	ctx := context.Background()
	client := dialThroughInterceptors(t, osServer())
	kernel, initrd := []byte("kernel-bytes"), []byte("initrd-bytes")

	resp, err := addBootSet(t, client, wholeSet("mine", kernel, initrd)...)
	require.NoError(t, err)
	assert.Equal(t, "mine", resp.GetBootSet().GetName())
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED, resp.GetBootSet().GetOrigin())
	assert.Equal(t, hexSum(kernel), resp.GetBootSet().GetKernelDigest())

	info, err := client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, info.GetBootSets(), 2)
	assert.Equal(t, "mine", info.GetBootSets()[0].GetName())
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED, info.GetBootSets()[0].GetOrigin())
	assert.Equal(t, hexSum(initrd), info.GetBootSets()[0].GetInitrdDigest())
	assert.Equal(t, "os", info.GetBootSets()[1].GetName())
	assert.Equal(t, v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_CONFIGURED, info.GetBootSets()[1].GetOrigin())

	again, err := addBootSet(t, client, wholeSet("mine", kernel, initrd)...)
	require.NoError(t, err, "the same digests under the same name are success")
	assert.Equal(t, resp.GetBootSet().GetKernelDigest(), again.GetBootSet().GetKernelDigest())
}

func TestAddBootSet_RefusalsReachTheCallerWithTheirKind(t *testing.T) {
	kernel, initrd := []byte("kernel-bytes"), []byte("initrd-bytes")
	for _, tc := range []struct {
		name string
		msgs []*v1alpha2.AddBootSetRequest
		kind v1alpha2.OutcomeKind
		text string
	}{
		{"a kernel that is not its digest", []*v1alpha2.AddBootSetRequest{
			startMessage("mine", []byte("other"), initrd), kernelChunk(kernel), initrdChunk(initrd)},
			v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "kernel"},
		{"an initrd that is not its digest", []*v1alpha2.AddBootSetRequest{
			startMessage("mine", kernel, []byte("other")), kernelChunk(kernel), initrdChunk(initrd)},
			v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "initrd"},
		{"a first message that is not the start", []*v1alpha2.AddBootSetRequest{kernelChunk(kernel)},
			v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "first message"},
		{"a kernel chunk after an initrd chunk", []*v1alpha2.AddBootSetRequest{
			startMessage("mine", kernel, initrd), kernelChunk(kernel), initrdChunk(initrd), kernelChunk(kernel)},
			v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "kernel chunk after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dialThroughInterceptors(t, osServer())
			_, err := addBootSet(t, client, tc.msgs...)
			assert.Equal(t, tc.kind, v1alpha2.KindOf(err), "%v", err)
			assert.ErrorContains(t, err, tc.text)

			info, err := client.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
			require.NoError(t, err)
			assert.Len(t, info.GetBootSets(), 1, "nothing was published")
			_, err = addBootSet(t, client, wholeSet("mine", kernel, initrd)...)
			require.NoError(t, err, "control: the whole set in order is accepted")
		})
	}
}

func TestAddBootSet_ATakenNameWithOtherDigestsIsRefusedAsAConflict(t *testing.T) {
	client := dialThroughInterceptors(t, osServer())
	_, err := addBootSet(t, client, wholeSet("mine", []byte("kernel-bytes"), []byte("initrd-bytes"))...)
	require.NoError(t, err)
	_, err = addBootSet(t, client, wholeSet("mine", []byte("kernel-other"), []byte("initrd-bytes"))...)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, v1alpha2.KindOf(err), "%v", err)
}

func TestRemoveBootSet_RefusesAConfiguredOrNamedSetAndRemovesTheOthers(t *testing.T) {
	ctx := context.Background()
	client := dialThroughInterceptors(t, osServer())
	kernel, initrd := []byte("kernel-bytes"), []byte("initrd-bytes")
	_, err := addBootSet(t, client, wholeSet("mine", kernel, initrd)...)
	require.NoError(t, err)

	_, err = client.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: "os"})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a configured set: %v", err)

	req := osRequest("m")
	req.OsBoot.BootSet = "mine"
	_, err = client.CreateMachine(ctx, req)
	require.NoError(t, err)
	_, err = client.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: "mine"})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a set a Machine names: %v", err)

	_, err = client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	_, err = client.RemoveBootSet(ctx, &v1alpha2.RemoveBootSetRequest{Name: "mine"})
	require.NoError(t, err)
	info, err := client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	assert.Len(t, info.GetBootSets(), 1, "only the configured set is left")
}
