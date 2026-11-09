// Package channel is the machine.Guest adapter: a client of the Guest Agent's
// gRPC contract, reaching the agent of a given Machine over the byte stream
// the Driver opens to its guest (guest.md, ADR 0014).
package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	guestv1alpha1 "github.com/garamsh/mvmd/daemon/internal/guestapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Connector opens a byte stream to a Machine's guest port. machine.Driver is
// one: whatever the VMM needs before the first byte is the Driver's.
type Connector interface {
	GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error)
}

// Guest is a machine.Guest over the Guest Agent's gRPC contract. It holds no
// connection: each call opens one through the Connector, for the Machine the
// call names, and closes it. A vsock connection resets when the guest is
// restored, so a call whose connection breaks before it is answered opens
// another and repeats once, which is safe because every call the agent serves
// is repeat-safe (guest.md). A stream that broke resumes where it was.
//
// The adapter puts no time limit of its own on a call: the lifecycle bounds a
// Boot, and a long output or file stream is the caller's to cut.
type Guest struct {
	conn Connector
}

var _ machine.Guest = (*Guest)(nil)

// NewGuest returns a Guest that reaches each Machine's agent through conn.
func NewGuest(conn Connector) *Guest { return &Guest{conn: conn} }

// operation is what a call does, which decides what a status of the agent
// means: the same NOT_FOUND is an exec for one and a path for another.
type operation int

const (
	opOther operation = iota
	opBoot
	opExec
	opFile
	opPatch
	opDisk
)

// errCut is a stream that ended before its last message: the connection
// broke between messages. A repeat is safe.
var errCut = errors.New("the stream ended before its last message")

// reconnect is the client's backoff between dials for opBoot. A refused dial
// costs one local socket exchange, and a guest boots in seconds, so the
// longest wait is one second: that is all a listening agent waits to be
// found, against a boot timeout of minutes.
var reconnect = backoff.Config{
	BaseDelay:  100 * time.Millisecond,
	Multiplier: 1.6,
	Jitter:     0.2,
	MaxDelay:   time.Second,
}

// notListeningError wraps a dial that never got an answer: the agent did not
// listen before the context ended.
type notListeningError struct{ err error }

func (e *notListeningError) Error() string { return e.err.Error() }
func (e *notListeningError) Unwrap() error { return e.err }

// dialError marks an error converted by the dialer: mapError recognizes it
// so it does not conflate a dial failure with an agent-returned status.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }
func (e *dialError) GRPCStatus() *status.Status {
	return status.New(codes.Internal, e.err.Error())
}

// run calls f with a client of the Machine's agent, and repeats it once, on a
// connection of its own, when the first connection broke before the call was
// answered. f is called again from the same variables, so a stream resumes
// from what it has already passed on.
func (g *Guest) run(ctx context.Context, id machine.MachineID, op operation, f func(guestv1alpha1.GuestServiceClient) error) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = g.once(ctx, id, op, f)
		if err == nil || ctx.Err() != nil || !reset(err) {
			break
		}
	}
	return mapError(ctx, op, err)
}

func (g *Guest) once(ctx context.Context, id machine.MachineID, op operation, f func(guestv1alpha1.GuestServiceClient) error) error {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dial context.Context, _ string) (net.Conn, error) {
			conn, dialErr := g.conn.GuestConn(dial, id, machine.GuestAgentPort)
			if dialErr != nil && errors.Is(dialErr, machine.ErrVMMEnded) && op == opBoot {
				// Return a gRPC status error with a non-retryable code so
				// grpc's retry mechanism stops immediately rather than
				// waiting for the backoff until the context ends.
				return nil, &dialError{fmt.Errorf("the VMM has ended: %w", dialErr)}
			}
			if dialErr != nil && op != opBoot {
				// For non-Boot calls, return an Internal status so gRPC
				// does not retry the dial in a background goroutine.
				return nil, &dialError{fmt.Errorf("dial failed: %w", dialErr)}
			}
			return conn, dialErr
		}),
	}
	if op == opBoot {
		opts = append(opts, grpc.WithConnectParams(grpc.ConnectParams{Backoff: reconnect}))
	}
	cc, err := grpc.NewClient("passthrough:///guest", opts...)
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	err = f(guestv1alpha1.NewGuestServiceClient(cc))
	if op == opBoot && err != nil && ctx.Err() != nil {
		err = &notListeningError{err}
	}
	return err
}

// reset reports whether the error is a connection that broke or never opened.
func reset(err error) bool {
	if errors.Is(err, errCut) {
		return true
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Unavailable
}

// bootStepNames are the names a boot step goes by in guest.md and on the
// console, by the contract's enum.
var bootStepNames = map[guestv1alpha1.BootStep]string{
	guestv1alpha1.BootStep_BOOT_STEP_CHECK:           "check the devices",
	guestv1alpha1.BootStep_BOOT_STEP_MOUNT_IMAGE:     "mount the image",
	guestv1alpha1.BootStep_BOOT_STEP_MOUNT_ROOT_DISK: "mount the root disk",
	guestv1alpha1.BootStep_BOOT_STEP_OVERLAY:         "overlay the root disk",
	guestv1alpha1.BootStep_BOOT_STEP_SWITCH_ROOT:     "switch root",
	guestv1alpha1.BootStep_BOOT_STEP_NETWORK:         "configure the network",
	guestv1alpha1.BootStep_BOOT_STEP_ENTRYPOINT:      "start the entrypoint",
}

// mapError turns what the agent or the connection answered into the errors the
// Guest port promises. A status the agent means a client to act on carries a
// detail message in the contract (guest.proto, "Status details"), and the
// detail's type decides the mapping, never the status message, which is for
// people. A status with no detail we know maps by its code and the call alone.
func mapError(ctx context.Context, op operation, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errCut) {
		return fmt.Errorf("%w: %v", machine.ErrGuestUnreachable, err)
	}
	if errors.Is(err, errNotRepeatable) {
		return fmt.Errorf("%w: the content was already read", machine.ErrGuestUnreachable)
	}
	// The dialer turns dial errors into Internal status so gRPC does not retry
	// in a background goroutine; opBoot maps Internal back to ErrVMMENDED
	// (after the notListening block), and other ops map it to ErrGuestUnreachable.
	// Dialer-converted errors carry a dialError marker so mapError can
	// distinguish them from agent-returned Internal statuses.
	var didDial *dialError
	errors.As(err, &didDial)
	st, ok := status.FromError(err)
	if ok && st.Code() == codes.Internal {
		if didDial != nil {
			if op == opBoot {
				return fmt.Errorf("%w: the agent VMM has ended", machine.ErrVMMEnded)
			}
			return fmt.Errorf("%w: dial failed", machine.ErrGuestUnreachable)
		}
		if op == opBoot {
			var notListening *notListeningError
			if errors.As(err, &notListening) {
				return fmt.Errorf("%w: the agent VMM has ended", machine.ErrVMMEnded)
			}
			// Agent-returned Internal without notListening: fall through to
			// detail parsing and switch below (e.g. BootFailed → *BootStepError).
		}
	}
	if op == opBoot {
		var notListening *notListeningError
		if errors.As(err, &notListening) {
			st, ok := status.FromError(notListening)
			if ok && st.Code() == codes.Internal {
				return fmt.Errorf("%w: the agent VMM has ended", machine.ErrVMMEnded)
			}
		}
		if errors.As(err, &notListening) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: the agent did not listen before the boot's time ended: %w",
				machine.ErrGuestUnreachable, ctx.Err())
		}
	}
	msg := st.Message()
	var notBooted bool
	for _, d := range st.Details() {
		switch d := d.(type) {
		case *guestv1alpha1.ExecOutputGone:
			return &machine.OutputGoneError{Earliest: d.GetEarliestOffset()}
		case *guestv1alpha1.BootFailed:
			step, known := bootStepNames[d.GetStep()]
			if !known {
				step = d.GetStep().String()
			}
			return &machine.BootStepError{Step: step, Detail: d.GetDetail()}
		case *guestv1alpha1.WrongFileKind:
			if d.GetExpected() == guestv1alpha1.EntryKind_ENTRY_KIND_FILE {
				return fmt.Errorf("%w: %s", machine.ErrNotRegular, d.GetPath())
			}
			return fmt.Errorf("guest agent: %s is not a %s: %s", d.GetPath(), d.GetExpected(), msg)
		case *guestv1alpha1.DiskInUse:
			return fmt.Errorf("%w: %s", machine.ErrDiskInUse, d.GetDiskId())
		case *guestv1alpha1.NotBooted:
			notBooted = true
		}
	}
	switch st.Code() {
	case codes.Unavailable:
		return fmt.Errorf("%w: %s", machine.ErrGuestUnreachable, msg)
	case codes.Canceled:
		return fmt.Errorf("%w: %s", context.Canceled, msg)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
	case codes.NotFound:
		switch op {
		case opExec:
			return fmt.Errorf("%w: %s", machine.ErrExecAbsent, msg)
		case opFile, opPatch:
			return fmt.Errorf("%w: %s", machine.ErrFileAbsent, msg)
		case opDisk:
			return fmt.Errorf("%w: %s", machine.ErrDiskUnknown, msg)
		}
	case codes.FailedPrecondition:
		if op == opPatch && !notBooted {
			return fmt.Errorf("%w: %s", machine.ErrPatchPrecondition, msg)
		}
	case codes.Internal:
		if op == opBoot {
			return fmt.Errorf("%w: the agent VMM has ended", machine.ErrVMMEnded)
		}
	}
	return fmt.Errorf("guest agent answered %s: %s", st.Code(), msg)
}

// networkConfig is the Machine's whole network as the agent takes it: each
// family served gets its address with a prefix length and its gateway. A
// Machine holds one host address per family (ADR 0011), so the prefix is the
// whole address, /32 and /128.
func networkConfig(n machine.GuestNetwork) *guestv1alpha1.NetworkConfig {
	out := &guestv1alpha1.NetworkConfig{}
	for _, a := range n.Addresses {
		a = a.Unmap()
		cfg := netip.PrefixFrom(a, a.BitLen()).String()
		switch {
		case a.Is4() && out.Ipv4 == nil:
			out.Ipv4 = &guestv1alpha1.Ipv4Config{Address: cfg, Gateway: gateway(n.Gateways, true)}
		case a.Is6() && out.Ipv6 == nil:
			out.Ipv6 = &guestv1alpha1.Ipv6Config{Address: cfg, Gateway: gateway(n.Gateways, false)}
		}
	}
	for _, r := range n.Resolvers {
		out.Resolvers = append(out.Resolvers, r.String())
	}
	return out
}

// gateway is the first gateway of the family, or empty.
func gateway(gateways []netip.Addr, v4 bool) string {
	for _, g := range gateways {
		if g.Unmap().Is4() == v4 {
			return g.Unmap().String()
		}
	}
	return ""
}

// execRecord converts the agent's record, refusing a status this daemon does
// not know instead of guessing one.
func execRecord(r *guestv1alpha1.ExecRecord) (machine.ExecRecord, error) {
	var s machine.ExecStatus
	switch r.GetStatus() {
	case guestv1alpha1.ExecStatus_EXEC_STATUS_NOT_STARTED:
		s = machine.ExecNotStarted
	case guestv1alpha1.ExecStatus_EXEC_STATUS_STARTED:
		s = machine.ExecStarted
	case guestv1alpha1.ExecStatus_EXEC_STATUS_EXITED:
		s = machine.ExecExited
	case guestv1alpha1.ExecStatus_EXEC_STATUS_CUT_SHORT:
		s = machine.ExecCutShort
	default:
		return machine.ExecRecord{}, fmt.Errorf("the guest agent sent exec status %v, which this daemon does not know", r.GetStatus())
	}
	return machine.ExecRecord{
		ID:         machine.ExecID(r.GetExecId()),
		Status:     s,
		Reason:     r.GetReason(),
		ExitCode:   r.GetExitCode(),
		TermSignal: r.GetTermSignal(),
	}, nil
}

// Health implements machine.Guest.
func (g *Guest) Health(ctx context.Context, id machine.MachineID) error {
	return g.run(ctx, id, opOther, func(c guestv1alpha1.GuestServiceClient) error {
		_, err := c.Health(ctx, &guestv1alpha1.HealthRequest{})
		return err
	})
}

// Boot implements machine.Guest. A boot step that failed is a
// *machine.BootStepError.
func (g *Guest) Boot(ctx context.Context, id machine.MachineID, boot machine.GuestBoot) error {
	req := &guestv1alpha1.BootRequest{
		ImageUuid:    boot.ImageUUID,
		RootDiskUuid: boot.RootDiskUUID,
		Run: &guestv1alpha1.RunConfig{
			Argv:       boot.Run.Command,
			Env:        boot.Run.Env,
			WorkingDir: boot.Run.WorkingDir,
		},
		Network: networkConfig(boot.Network),
	}
	return g.run(ctx, id, opBoot, func(c guestv1alpha1.GuestServiceClient) error {
		_, err := c.Boot(ctx, req, grpc.WaitForReady(true))
		return err
	})
}

// CatchUp implements machine.Guest.
func (g *Guest) CatchUp(ctx context.Context, id machine.MachineID, catchUp machine.CatchUp) error {
	req := &guestv1alpha1.CatchUpRequest{
		RestoreId:     string(catchUp.RestoreID),
		Entropy:       catchUp.Entropy,
		HostUnixNanos: catchUp.HostTime.UnixNano(),
		Network:       networkConfig(catchUp.Network),
	}
	return g.run(ctx, id, opOther, func(c guestv1alpha1.GuestServiceClient) error {
		_, err := c.CatchUp(ctx, req)
		return err
	})
}

// Exec implements machine.Guest.
func (g *Guest) Exec(ctx context.Context, id machine.MachineID, exec machine.ExecID, spec machine.ExecSpec) (machine.ExecRecord, error) {
	var rec machine.ExecRecord
	err := g.run(ctx, id, opExec, func(c guestv1alpha1.GuestServiceClient) error {
		resp, err := c.Exec(ctx, &guestv1alpha1.ExecRequest{
			ExecId:     string(exec),
			Argv:       spec.Command,
			Env:        spec.Env,
			WorkingDir: spec.WorkingDir,
		})
		if err != nil {
			return err
		}
		rec, err = execRecord(resp.GetRecord())
		return err
	})
	return rec, err
}

// ReadExecOutput implements machine.Guest. A stream that breaks is resumed
// after the last byte passed on.
func (g *Guest) ReadExecOutput(ctx context.Context, id machine.MachineID, exec machine.ExecID, after uint64, send func(machine.OutputChunk) error) (machine.ExecRecord, error) {
	var record machine.ExecRecord
	next := after
	err := g.run(ctx, id, opExec, func(c guestv1alpha1.GuestServiceClient) error {
		stream, err := c.ReadExecOutput(ctx, &guestv1alpha1.ReadExecOutputRequest{ExecId: string(exec), AfterOffset: next})
		if err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return errCut
			}
			if err != nil {
				return err
			}
			switch p := msg.GetPart().(type) {
			case *guestv1alpha1.ReadExecOutputResponse_Chunk:
				var s machine.OutputStream
				switch p.Chunk.GetStream() {
				case guestv1alpha1.OutputStream_OUTPUT_STREAM_STDOUT:
					s = machine.OutputStdout
				case guestv1alpha1.OutputStream_OUTPUT_STREAM_STDERR:
					s = machine.OutputStderr
				default:
					return fmt.Errorf("the guest agent sent output stream %v, which this daemon does not know", p.Chunk.GetStream())
				}
				if err := send(machine.OutputChunk{Stream: s, Offset: p.Chunk.GetOffset(), Data: p.Chunk.GetData()}); err != nil {
					return err
				}
				next = p.Chunk.GetOffset() + uint64(len(p.Chunk.GetData()))
			case *guestv1alpha1.ReadExecOutputResponse_Record:
				record, err = execRecord(p.Record)
				return err
			}
		}
	})
	return record, err
}

// ReadFile implements machine.Guest. A stream that breaks is read again and
// the bytes already passed on are skipped.
func (g *Guest) ReadFile(ctx context.Context, id machine.MachineID, path string, send func(chunk []byte) error) error {
	var sent uint64
	return g.run(ctx, id, opFile, func(c guestv1alpha1.GuestServiceClient) error {
		stream, err := c.ReadFile(ctx, &guestv1alpha1.ReadFileRequest{Path: path})
		if err != nil {
			return err
		}
		var seen uint64
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			chunk := msg.GetChunk()
			start := seen
			seen += uint64(len(chunk))
			if seen <= sent {
				continue // all of it was passed on before the break
			}
			if start < sent {
				chunk = chunk[sent-start:]
			}
			if err := send(chunk); err != nil {
				return err
			}
			sent += uint64(len(chunk))
		}
	})
}

// WriteFile implements machine.Guest. The content is sent with its digest, so
// the agent keeps the old file unless every byte arrived. A reader is
// consumed once: a connection that breaks after the first read is not
// repeated, and the caller, who holds the content, repeats the call.
func (g *Guest) WriteFile(ctx context.Context, id machine.MachineID, path string, content io.Reader) error {
	consumed := false
	err := g.run(ctx, id, opFile, func(c guestv1alpha1.GuestServiceClient) error {
		if consumed {
			return errNotRepeatable
		}
		stream, err := c.WriteFile(ctx)
		if err != nil {
			return err
		}
		if err := stream.Send(&guestv1alpha1.WriteFileRequest{Part: &guestv1alpha1.WriteFileRequest_Start{Start: &guestv1alpha1.WriteFileStart{Path: path}}}); err != nil {
			return sendFailure(stream, err)
		}
		hash := sha256.New()
		buf := make([]byte, 32<<10)
		for {
			consumed = true
			n, readErr := content.Read(buf)
			if n > 0 {
				hash.Write(buf[:n])
				if err := stream.Send(&guestv1alpha1.WriteFileRequest{Part: &guestv1alpha1.WriteFileRequest_Chunk{Chunk: buf[:n]}}); err != nil {
					return sendFailure(stream, err)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				// Dropping the stream without its end leaves the old file whole.
				return fmt.Errorf("read the content: %w", readErr)
			}
		}
		end := &guestv1alpha1.WriteFileEnd{Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil))}
		if err := stream.Send(&guestv1alpha1.WriteFileRequest{Part: &guestv1alpha1.WriteFileRequest_End{End: end}}); err != nil {
			return sendFailure(stream, err)
		}
		_, err = stream.CloseAndRecv()
		return err
	})
	if errors.Is(err, errNotRepeatable) {
		return machine.ErrGuestUnreachable
	}
	return err
}

// errNotRepeatable stops a WriteFile whose reader has been read.
var errNotRepeatable = errors.New("the content was already read")

// sendFailure returns the status that ended the stream: Send reports only EOF
// when the server has answered, and the answer is on CloseAndRecv.
func sendFailure(stream grpc.ClientStreamingClient[guestv1alpha1.WriteFileRequest, guestv1alpha1.WriteFileResponse], err error) error {
	if errors.Is(err, io.EOF) {
		if _, recvErr := stream.CloseAndRecv(); recvErr != nil {
			return recvErr
		}
	}
	return err
}

// PatchFile implements machine.Guest.
func (g *Guest) PatchFile(ctx context.Context, id machine.MachineID, path, baseDigest, expectedDigest string, patch []byte) (machine.PatchOutcome, error) {
	var outcome machine.PatchOutcome
	err := g.run(ctx, id, opPatch, func(c guestv1alpha1.GuestServiceClient) error {
		resp, err := c.PatchFile(ctx, &guestv1alpha1.PatchFileRequest{
			Path:           path,
			BaseDigest:     baseDigest,
			ExpectedDigest: expectedDigest,
			Patch:          patch,
		})
		if err != nil {
			return err
		}
		switch resp.GetOutcome() {
		case guestv1alpha1.PatchOutcome_PATCH_OUTCOME_APPLIED:
			outcome = machine.PatchApplied
		case guestv1alpha1.PatchOutcome_PATCH_OUTCOME_ALREADY_APPLIED:
			outcome = machine.PatchAlreadyApplied
		default:
			return fmt.Errorf("the guest agent sent patch outcome %v, which this daemon does not know", resp.GetOutcome())
		}
		return nil
	})
	return outcome, err
}

// Terminate implements machine.Guest. The request carries no signal (ADR 0018).
func (g *Guest) Terminate(ctx context.Context, id machine.MachineID) error {
	return g.run(ctx, id, opOther, func(c guestv1alpha1.GuestServiceClient) error {
		_, err := c.Terminate(ctx, &guestv1alpha1.TerminateRequest{})
		return err
	})
}

// FindDisk implements machine.Guest (ADR 0022). The device the agent found is
// not returned: the daemon records no guest device name.
func (g *Guest) FindDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID) error {
	return g.run(ctx, id, opDisk, func(c guestv1alpha1.GuestServiceClient) error {
		_, err := c.FindDisk(ctx, &guestv1alpha1.FindDiskRequest{DiskId: string(disk)})
		return err
	})
}

// ReleaseDisk implements machine.Guest (ADR 0022).
func (g *Guest) ReleaseDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID) error {
	return g.run(ctx, id, opDisk, func(c guestv1alpha1.GuestServiceClient) error {
		_, err := c.ReleaseDisk(ctx, &guestv1alpha1.ReleaseDiskRequest{DiskId: string(disk)})
		return err
	})
}

// SearchFiles implements machine.Guest.
func (g *Guest) SearchFiles(ctx context.Context, id machine.MachineID, path, pattern string) ([]string, error) {
	var paths []string
	err := g.run(ctx, id, opFile, func(c guestv1alpha1.GuestServiceClient) error {
		paths = nil // a repeat lists again from the start
		stream, err := c.SearchFiles(ctx, &guestv1alpha1.SearchFilesRequest{Path: path, Pattern: pattern})
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
