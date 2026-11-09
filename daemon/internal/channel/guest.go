// Package channel is a machine.Guest over a vsock-backed gRPC connection to a
// Guest Agent. It opens the connection on each call, so repeated invocations
// are repeat-safe by construction.
package channel

import (
	"context"
	"io"
	"net"
	"net/netip"
	"time"

	guestv1alpha1 "github.com/garamsh/mvmd/daemon/internal/guestapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Dialer creates a gRPC client from a vsock connection. The machine.Guest
// interface is the public contract; the dialer is the Driver's way of
// creating one.
type Dialer func(conn net.Conn) (guestv1alpha1.GuestServiceClient, error)

// dialConn opens a transport connection to the Guest Agent.
type dialConn func() (net.Conn, error)

// Guest is the machine.Guest implemented over a vsock gRPC connection to the
// Guest Agent. It opens the connection on each call, so repeated invocations
// are safe and the agent always sees a fresh start.
type Guest struct {
	dialer   Dialer
	dialConn dialConn
}

var _ machine.Guest = (*Guest)(nil)

// NewGuest returns a Guest that opens a fresh vsock connection on each call
// using the provided dialer, so the agent is always reached anew.
func NewGuest(dialer Dialer) *Guest {
	return &Guest{
		dialer:   dialer,
		dialConn: func() (net.Conn, error) { return net.Dial("vsock", "auto:5") },
	}
}

// dial opens a transport connection and returns a gRPC client, or an error if
// the connection could not be opened. The caller owns the underlying net.Conn
// and must close it when done.
func (g *Guest) dial() (net.Conn, guestv1alpha1.GuestServiceClient, error) {
	conn, err := g.dialConn()
	if err != nil {
		return nil, nil, err
	}
	client, err := g.dialer(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, client, nil
}

func (g *Guest) call(ctx context.Context, f func(guestv1alpha1.GuestServiceClient) error) error {
	conn, client, err := g.dial()
	if err != nil {
		return machine.ErrGuestUnreachable
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return f(client)
}

// Health implements machine.Guest.
func (g *Guest) Health(ctx context.Context, _ machine.MachineID) error {
	return g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		_, err := client.Health(ctx, &guestv1alpha1.HealthRequest{})
		return err
	})
}

// Boot implements machine.Guest.
func (g *Guest) Boot(ctx context.Context, _ machine.MachineID, boot machine.GuestBoot) error {
	resolvers := make([]string, len(boot.Network.Resolvers))
	for i, r := range boot.Network.Resolvers {
		resolvers[i] = netip.Addr(r).String()
	}
	return g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		_, err := client.Boot(ctx, &guestv1alpha1.BootRequest{
			ImageUuid:    boot.ImageUUID,
			RootDiskUuid: boot.RootDiskUUID,
			Run: &guestv1alpha1.RunConfig{
				Argv:       boot.Run.Command,
				Env:        boot.Run.Env,
				WorkingDir: boot.Run.WorkingDir,
			},
			Network: &guestv1alpha1.NetworkConfig{
				Resolvers: resolvers,
			},
		})
		return err
	})
}

// CatchUp implements machine.Guest.
func (g *Guest) CatchUp(ctx context.Context, _ machine.MachineID, catchUp machine.CatchUp) error {
	return g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		_, err := client.CatchUp(ctx, &guestv1alpha1.CatchUpRequest{
			RestoreId:     string(catchUp.RestoreID),
			Entropy:       catchUp.Entropy,
			HostUnixNanos: catchUp.HostTime.UnixNano(),
			Network:       &guestv1alpha1.NetworkConfig{},
		})
		return err
	})
}

// Exec implements machine.Guest.
func (g *Guest) Exec(ctx context.Context, _ machine.MachineID, exec machine.ExecID, spec machine.ExecSpec) (machine.ExecRecord, error) {
	var rec machine.ExecRecord
	err := g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		resp, err := client.Exec(ctx, &guestv1alpha1.ExecRequest{
			ExecId:     string(exec),
			Argv:       spec.Command,
			Env:        spec.Env,
			WorkingDir: spec.WorkingDir,
		})
		if err != nil {
			return err
		}
		r := resp.GetRecord()
		rec = machine.ExecRecord{
			ID:         machine.ExecID(r.GetExecId()),
			Status:     machine.ExecStatus(r.GetStatus()),
			Reason:     r.GetReason(),
			ExitCode:   r.GetExitCode(),
			TermSignal: r.GetTermSignal(),
		}
		return nil
	})
	return rec, err
}

// ReadExecOutput implements machine.Guest.
func (g *Guest) ReadExecOutput(ctx context.Context, _ machine.MachineID, exec machine.ExecID, after uint64, send func(machine.OutputChunk) error) (machine.ExecRecord, error) {
	var record machine.ExecRecord
	err := g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		stream, err := client.ReadExecOutput(ctx, &guestv1alpha1.ReadExecOutputRequest{
			ExecId:      string(exec),
			AfterOffset: after,
		})
		if err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			switch p := msg.GetPart().(type) {
			case *guestv1alpha1.ReadExecOutputResponse_Chunk:
				c := p.Chunk
				if err := send(machine.OutputChunk{
					Stream: machine.OutputStream(c.GetStream()),
					Offset: c.GetOffset(),
					Data:   c.GetData(),
				}); err != nil {
					return err
				}
			case *guestv1alpha1.ReadExecOutputResponse_Record:
				r := p.Record
				record = machine.ExecRecord{
					ID:         machine.ExecID(r.GetExecId()),
					Status:     machine.ExecStatus(r.GetStatus()),
					Reason:     r.GetReason(),
					ExitCode:   r.GetExitCode(),
					TermSignal: r.GetTermSignal(),
				}
				return nil
			}
		}
	})
	return record, err
}

// ReadFile implements machine.Guest.
func (g *Guest) ReadFile(ctx context.Context, _ machine.MachineID, path string, send func(chunk []byte) error) error {
	return g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		stream, err := client.ReadFile(ctx, &guestv1alpha1.ReadFileRequest{
			Path: path,
		})
		if err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err := send(msg.GetChunk()); err != nil {
				return err
			}
		}
	})
}

// WriteFile implements machine.Guest.
func (g *Guest) WriteFile(ctx context.Context, _ machine.MachineID, path string, content io.Reader) error {
	return g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		stream, err := client.WriteFile(ctx)
		if err != nil {
			return err
		}
		if err := stream.Send(&guestv1alpha1.WriteFileRequest{
			Part: &guestv1alpha1.WriteFileRequest_Start{
				Start: &guestv1alpha1.WriteFileStart{Path: path},
			},
		}); err != nil {
			return err
		}
		buf := make([]byte, 4096)
		for {
			n, readErr := content.Read(buf)
			if n > 0 {
				if err := stream.Send(&guestv1alpha1.WriteFileRequest{
					Part: &guestv1alpha1.WriteFileRequest_Chunk{
						Chunk: buf[:n],
					},
				}); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		return stream.Send(&guestv1alpha1.WriteFileRequest{
			Part: &guestv1alpha1.WriteFileRequest_End{
				End: &guestv1alpha1.WriteFileEnd{},
			},
		})
	})
}

// PatchFile implements machine.Guest.
func (g *Guest) PatchFile(ctx context.Context, _ machine.MachineID, path, baseDigest, expectedDigest string, patch []byte) (machine.PatchOutcome, error) {
	var outcome machine.PatchOutcome
	err := g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		resp, err := client.PatchFile(ctx, &guestv1alpha1.PatchFileRequest{
			Path:           path,
			BaseDigest:     baseDigest,
			ExpectedDigest: expectedDigest,
			Patch:          patch,
		})
		if err != nil {
			return err
		}
		outcome = machine.PatchOutcome(resp.GetOutcome())
		return nil
	})
	return outcome, err
}

// SearchFiles implements machine.Guest.
func (g *Guest) SearchFiles(ctx context.Context, _ machine.MachineID, path, pattern string) ([]string, error) {
	var paths []string
	err := g.call(ctx, func(client guestv1alpha1.GuestServiceClient) error {
		stream, err := client.SearchFiles(ctx, &guestv1alpha1.SearchFilesRequest{
			Path:    path,
			Pattern: pattern,
		})
		if err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			for _, e := range msg.GetMatches() {
				paths = append(paths, e.GetPath())
			}
		}
	})
	return paths, err
}
