package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// apiError is Firecracker's refusal of a request: its status and the
// fault_message of its body.
type apiError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("firecracker refused %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// client is Firecracker's HTTP API on a Unix socket. Firecracker serves one
// connection at a time, so no connection is kept: each request opens its own
// and closes it.
type client struct {
	http *http.Client
}

func newClient(socket string) *client {
	return &client{http: &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return unixsock.DialContext(ctx, socket)
		},
	}}}
}

// do sends one request on ctx and decodes a JSON answer into out when it is
// not nil. A status other than 2xx is an *apiError.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the body of %s %s: %w", method, path, err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read the answer to %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		var fault struct {
			Message string `json:"fault_message"`
		}
		_ = json.Unmarshal(raw, &fault)
		return &apiError{Method: method, Path: path, Status: resp.StatusCode, Message: fault.Message}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode the answer to %s %s: %w", method, path, err)
		}
	}
	return nil
}
