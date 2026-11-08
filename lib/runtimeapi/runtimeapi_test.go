package runtimeapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const validDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const fileChunk = 64 << 10

type server struct {
	v1alpha1.UnimplementedRuntimeServiceServer
	pulls atomic.Int32
	mu    sync.Mutex
	files map[string][]byte
}

func (s *server) WriteFile(stream grpc.ClientStreamingServer[v1alpha1.WriteFileRequest, v1alpha1.WriteFileResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	path := first.GetStart().GetPath()
	var content []byte
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		content = append(content, msg.GetChunk()...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = content
	return stream.SendAndClose(&v1alpha1.WriteFileResponse{})
}

func (s *server) ReadFile(req *v1alpha1.ReadFileRequest, stream grpc.ServerStreamingServer[v1alpha1.ReadFileResponse]) error {
	s.mu.Lock()
	content := s.files[req.GetPath()]
	s.mu.Unlock()
	for len(content) > 0 {
		n := min(fileChunk, len(content))
		if err := stream.Send(&v1alpha1.ReadFileResponse{Chunk: content[:n]}); err != nil {
			return err
		}
		content = content[n:]
	}
	return nil
}

func (s *server) GetRuntimeInfo(context.Context, *v1alpha1.GetRuntimeInfoRequest) (*v1alpha1.GetRuntimeInfoResponse, error) {
	return &v1alpha1.GetRuntimeInfoResponse{ApiVersions: []string{"mvmd.runtime.v1alpha1"}}, nil
}

func (s *server) PullImage(context.Context, *v1alpha1.PullImageRequest) (*v1alpha1.PullImageResponse, error) {
	s.pulls.Add(1)
	return &v1alpha1.PullImageResponse{}, nil
}

// serve starts s on a Unix socket in a short temporary directory and returns
// the socket path.
func serve(t *testing.T, s *server) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "runtime.sock")
	lis, err := net.Listen("unix", path)
	require.NoError(t, err)
	srv := grpc.NewServer()
	v1alpha1.RegisterRuntimeServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return path
}

func dial(t *testing.T, path string) v1alpha1.RuntimeServiceClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := runtimeapi.Dial(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return v1alpha1.NewRuntimeServiceClient(conn)
}

func TestDial_RoundTrip(t *testing.T) {
	client := dial(t, serve(t, &server{}))

	resp, err := client.GetRuntimeInfo(context.Background(), &v1alpha1.GetRuntimeInfoRequest{})

	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd.runtime.v1alpha1"}, resp.GetApiVersions())
}

func TestDial_RefusesTagImageReference(t *testing.T) {
	s := &server{}
	client := dial(t, serve(t, s))

	_, err := client.PullImage(context.Background(), &v1alpha1.PullImageRequest{Image: &v1alpha1.ImageRef{Repository: "app:latest", Digest: validDigest}})
	require.Error(t, err)
	assert.Equal(t, v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha1.KindOf(err))
	assert.Zero(t, s.pulls.Load(), "a refused request must not reach the server")

	_, err = client.PullImage(context.Background(), &v1alpha1.PullImageRequest{Image: &v1alpha1.ImageRef{Repository: "registry:5000/app", Digest: validDigest}})
	require.NoError(t, err)
	assert.Equal(t, int32(1), s.pulls.Load(), "the digest form must reach the server")
}

func TestFiles_RoundTripAboveDefaultMessageLimit(t *testing.T) {
	client := dial(t, serve(t, &server{}))
	content := make([]byte, 5<<20)
	for i := range content {
		content[i] = byte(i * 7)
	}

	writer, err := client.WriteFile(context.Background())
	require.NoError(t, err)
	require.NoError(t, writer.Send(&v1alpha1.WriteFileRequest{Part: &v1alpha1.WriteFileRequest_Start{Start: &v1alpha1.WriteFileStart{MachineId: "m", Path: "/big"}}}))
	for rest := content; len(rest) > 0; {
		n := min(fileChunk, len(rest))
		require.NoError(t, writer.Send(&v1alpha1.WriteFileRequest{Part: &v1alpha1.WriteFileRequest_Chunk{Chunk: rest[:n]}}))
		rest = rest[n:]
	}
	_, err = writer.CloseAndRecv()
	require.NoError(t, err)

	reader, err := client.ReadFile(context.Background(), &v1alpha1.ReadFileRequest{MachineId: "m", Path: "/big"})
	require.NoError(t, err)
	var got []byte
	for {
		msg, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		got = append(got, msg.GetChunk()...)
	}
	assert.Greater(t, len(got), 4<<20)
	assert.True(t, bytes.Equal(content, got), "content read back differs from content written")
}

func TestDial_RefusesSocketWithoutPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses socket permissions")
	}
	path := serve(t, &server{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, os.Chmod(path, 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	conn, err := runtimeapi.Dial(ctx, path)
	require.NoError(t, err, "the owner must connect")
	_ = conn.Close()

	require.NoError(t, os.Chmod(path, 0))
	info, err = os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0), info.Mode().Perm())
	_, err = runtimeapi.Dial(ctx, path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrPermission), "got %v", err)
}
