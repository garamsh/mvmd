package api_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// dialThroughInterceptors serves srv behind the runtime API's interceptors and
// returns a client of it.
func dialThroughInterceptors(t *testing.T, srv *api.Server) v1alpha2.RuntimeServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer(grpc.UnaryInterceptor(api.Interceptor), grpc.StreamInterceptor(api.StreamInterceptor))
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return v1alpha2.NewRuntimeServiceClient(conn)
}

func TestCreateMachine_OSFormPassesTheInterceptorsAndReachesTheService(t *testing.T) {
	ctx := context.Background()
	client := dialThroughInterceptors(t, osServer())

	created, err := client.CreateMachine(ctx, osRequest("m"))
	require.NoError(t, err)
	assert.Equal(t, "os", created.GetMachine().GetOsBoot().GetBootSet(), "the service created the OS-form Machine")

	imageForm := &v1alpha2.CreateMachineRequest{
		MachineId: "i", RequestId: "r-i",
		Resources: &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
	}
	_, err = client.CreateMachine(ctx, imageForm)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "control: an image form with an empty digest is refused")
	assert.ErrorContains(t, err, "is not a sha256 digest", "control: the interceptor's check refused it")
}
