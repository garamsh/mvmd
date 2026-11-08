// Package runtimeapi dials the mvmd runtime API on its Unix socket. The
// service stubs live in the version packages beside it, such as v1alpha1.
package runtimeapi

import (
	"context"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// validator is implemented by request messages that check their own fields.
type validator interface{ Validate() error }

// Dial connects to the runtime API on the Unix socket at path and returns
// once the connection is established. A request that fails its own Validate
// is refused with an InvalidArgument status before it is sent. Authentication is the
// socket's filesystem permissions; no credentials are sent.
func Dial(ctx context.Context, path string) (*grpc.ClientConn, error) {
	var (
		mu      sync.Mutex
		dialErr error
	)
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", addr)
		mu.Lock()
		dialErr = err
		mu.Unlock()
		return conn, err
	}
	conn, err := grpc.NewClient("passthrough:///"+path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithUnaryInterceptor(validateRequest),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if state == connectivity.TransientFailure || !conn.WaitForStateChange(ctx, state) {
			_ = conn.Close()
			mu.Lock()
			err := dialErr
			mu.Unlock()
			if err == nil {
				err = ctx.Err()
			}
			return nil, fmt.Errorf("dial %s: %w", path, err)
		}
	}
	return conn, nil
}

func validateRequest(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if v, ok := req.(validator); ok {
		if err := v.Validate(); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}
