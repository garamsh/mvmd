package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// feed is a server stream whose received message is a fixed one.
type feed struct {
	grpc.ServerStream
	msg proto.Message
}

func (f feed) Context() context.Context { return context.Background() }

func (f feed) RecvMsg(m any) error {
	proto.Merge(m.(proto.Message), f.msg)
	return nil
}

// receive runs the interceptor over a stream carrying msg, with a handler that
// receives one message as the generated server code does.
func receive(t *testing.T, msg proto.Message) (err error, handled bool) {
	t.Helper()
	err = api.StreamInterceptor(nil, feed{msg: msg}, &grpc.StreamServerInfo{}, func(_ any, ss grpc.ServerStream) error {
		handled = true
		return ss.RecvMsg(&v1alpha1.CreateMachineRequest{})
	})
	return err, handled
}

// No streaming request of the runtime API has a Validate today, so the
// interceptor is tested over a message that has one.
func TestStreamInterceptor_RefusesAnInvalidReceivedMessageAsRejected(t *testing.T) {
	invalid := &v1alpha1.CreateMachineRequest{}
	require.Error(t, invalid.Validate(), "the setup: the message is invalid")
	valid := &v1alpha1.CreateMachineRequest{
		MachineId:     "m",
		RequestId:     "r",
		ImageDigest:   "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Resources:     &v1alpha1.Resources{CpuCount: 1, MemoryBytes: 1 << 20},
		RootDiskBytes: 1 << 20,
	}
	require.NoError(t, valid.Validate(), "the setup: the control is valid")

	err, handled := receive(t, invalid)
	assert.True(t, handled, "the handler ran, and its read of the message failed")
	assert.Equal(t, v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha1.KindOf(err))

	// Control: the same stream with a valid message is not refused.
	err, _ = receive(t, valid)
	assert.NoError(t, err)
}
