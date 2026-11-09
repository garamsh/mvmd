package channel_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/garamsh/mvmd/daemon/internal/channel"
	guestv1alpha1 "github.com/garamsh/mvmd/daemon/internal/guestapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

var errBoom = errors.New("injected failure")

// state is what one Machine's fake Guest Agent holds and was sent. Its
// connections are numbered from one, in the order the adapter opened them.
type state struct {
	mu      sync.Mutex
	conns   []net.Conn
	servers []*grpc.Server

	// dropOnConnect closes connection n before the agent answers on it.
	dropOnConnect func(n int) bool
	// fail is the status a method answers with, by method name.
	fail map[string]error

	boot       *guestv1alpha1.BootRequest
	catchUp    *guestv1alpha1.CatchUpRequest
	exec       *guestv1alpha1.ExecRequest
	execStatus guestv1alpha1.ExecStatus
	outStream  guestv1alpha1.OutputStream
	outAfter   []uint64
	patchOut   guestv1alpha1.PatchOutcome
	patch      *guestv1alpha1.PatchFileRequest
	paths      []string

	// output is the exec's bytes, sent in chunks of 100 bytes, and the
	// connection numbered 1 stops after holdAfter chunks until it is closed.
	output    []byte
	holdAfter int

	// file is the file's content, sent in chunks of fileChunk(n) bytes on
	// connection n; connection 1 stops after holdAfter chunks.
	file      []byte
	fileChunk func(n int) int

	// written is what WriteFile received, and the digest it came with.
	written []byte
	digest  string
	// writeGate holds a WriteFile of connection 1 after its first chunk.
	holdWrite bool
}

func (s *state) failure(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail[method]
}

func (s *state) closeConn(n int) {
	s.mu.Lock()
	c := s.conns[n-1]
	s.mu.Unlock()
	_ = c.Close()
}

func (s *state) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// serve is what a guest does with a stream of its port: it serves the Guest
// Agent's contract on it.
func (s *state) serve(c net.Conn) {
	s.mu.Lock()
	s.conns = append(s.conns, c)
	n := len(s.conns)
	drop := s.dropOnConnect != nil && s.dropOnConnect(n)
	s.mu.Unlock()
	if drop {
		_ = c.Close()
		return
	}
	srv := grpc.NewServer()
	guestv1alpha1.RegisterGuestServiceServer(srv, &agent{state: s, n: n})
	s.mu.Lock()
	s.servers = append(s.servers, srv)
	s.mu.Unlock()
	_ = srv.Serve(&oneConn{conn: c, done: make(chan struct{})})
}

// oneConn is a listener that yields one connection.
type oneConn struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *oneConn) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

type agent struct {
	guestv1alpha1.UnimplementedGuestServiceServer
	*state
	n int
}

func (a *agent) Health(context.Context, *guestv1alpha1.HealthRequest) (*guestv1alpha1.HealthResponse, error) {
	return &guestv1alpha1.HealthResponse{}, a.failure("Health")
}

func (a *agent) Boot(_ context.Context, r *guestv1alpha1.BootRequest) (*guestv1alpha1.BootResponse, error) {
	a.mu.Lock()
	a.boot = r
	a.mu.Unlock()
	if err := a.failure("Boot"); err != nil {
		return nil, err
	}
	return &guestv1alpha1.BootResponse{}, nil
}

func (a *agent) CatchUp(_ context.Context, r *guestv1alpha1.CatchUpRequest) (*guestv1alpha1.CatchUpResponse, error) {
	a.mu.Lock()
	a.catchUp = r
	a.mu.Unlock()
	if err := a.failure("CatchUp"); err != nil {
		return nil, err
	}
	return &guestv1alpha1.CatchUpResponse{}, nil
}

func (a *agent) Exec(_ context.Context, r *guestv1alpha1.ExecRequest) (*guestv1alpha1.ExecResponse, error) {
	a.mu.Lock()
	a.exec = r
	st := a.execStatus
	a.mu.Unlock()
	if err := a.failure("Exec"); err != nil {
		return nil, err
	}
	return &guestv1alpha1.ExecResponse{Record: &guestv1alpha1.ExecRecord{ExecId: r.GetExecId(), Status: st, Reason: "why", ExitCode: 3, TermSignal: 9}}, nil
}

func (a *agent) ReadExecOutput(r *guestv1alpha1.ReadExecOutputRequest, srv grpc.ServerStreamingServer[guestv1alpha1.ReadExecOutputResponse]) error {
	a.mu.Lock()
	a.outAfter = append(a.outAfter, r.GetAfterOffset())
	output, hold, stream := a.output, a.holdAfter, a.outStream
	a.mu.Unlock()
	if err := a.failure("ReadExecOutput"); err != nil {
		return err
	}
	sent := 0
	for off := r.GetAfterOffset(); off < uint64(len(output)); off += 100 {
		if a.n == 1 && hold > 0 && sent == hold {
			<-srv.Context().Done()
			return srv.Context().Err()
		}
		end := min(off+100, uint64(len(output)))
		if err := srv.Send(&guestv1alpha1.ReadExecOutputResponse{Part: &guestv1alpha1.ReadExecOutputResponse_Chunk{
			Chunk: &guestv1alpha1.OutputChunk{Stream: stream, Offset: off, Data: output[off:end]},
		}}); err != nil {
			return err
		}
		sent++
	}
	return srv.Send(&guestv1alpha1.ReadExecOutputResponse{Part: &guestv1alpha1.ReadExecOutputResponse_Record{
		Record: &guestv1alpha1.ExecRecord{ExecId: r.GetExecId(), Status: guestv1alpha1.ExecStatus_EXEC_STATUS_EXITED, ExitCode: 7},
	}})
}

func (a *agent) ReadFile(_ *guestv1alpha1.ReadFileRequest, srv grpc.ServerStreamingServer[guestv1alpha1.ReadFileResponse]) error {
	a.mu.Lock()
	file, hold, size := a.file, a.holdAfter, 100
	if a.fileChunk != nil {
		size = a.fileChunk(a.n)
	}
	a.mu.Unlock()
	if err := a.failure("ReadFile"); err != nil {
		return err
	}
	sent := 0
	for off := 0; off < len(file); off += size {
		if a.n == 1 && hold > 0 && sent == hold {
			<-srv.Context().Done()
			return srv.Context().Err()
		}
		if err := srv.Send(&guestv1alpha1.ReadFileResponse{Chunk: file[off:min(off+size, len(file))]}); err != nil {
			return err
		}
		sent++
	}
	return nil
}

func (a *agent) WriteFile(srv grpc.ClientStreamingServer[guestv1alpha1.WriteFileRequest, guestv1alpha1.WriteFileResponse]) error {
	var got []byte
	var digest string
	for {
		m, err := srv.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if c := m.GetChunk(); c != nil {
			got = append(got, c...)
			if a.n == 1 && a.holdWrite {
				<-srv.Context().Done()
				return srv.Context().Err()
			}
		}
		if e := m.GetEnd(); e != nil {
			digest = e.GetDigest()
		}
	}
	if err := a.failure("WriteFile"); err != nil {
		return err
	}
	a.mu.Lock()
	a.written, a.digest = got, digest
	a.mu.Unlock()
	return srv.SendAndClose(&guestv1alpha1.WriteFileResponse{})
}

func (a *agent) PatchFile(_ context.Context, r *guestv1alpha1.PatchFileRequest) (*guestv1alpha1.PatchFileResponse, error) {
	a.mu.Lock()
	a.patch = r
	out := a.patchOut
	a.mu.Unlock()
	if err := a.failure("PatchFile"); err != nil {
		return nil, err
	}
	return &guestv1alpha1.PatchFileResponse{Outcome: out}, nil
}

func (a *agent) SearchFiles(_ *guestv1alpha1.SearchFilesRequest, srv grpc.ServerStreamingServer[guestv1alpha1.SearchFilesResponse]) error {
	a.mu.Lock()
	paths := a.paths
	a.mu.Unlock()
	if err := a.failure("SearchFiles"); err != nil {
		return err
	}
	for _, p := range paths {
		if err := srv.Send(&guestv1alpha1.SearchFilesResponse{Matches: []*guestv1alpha1.FileEntry{{Path: p}}}); err != nil {
			return err
		}
	}
	return nil
}

// harness is a Driver whose Machines have a fake Guest Agent each, and the
// adapter over it.
type harness struct {
	driver *memory.Driver
	guest  *channel.Guest
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	d := memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1"})
	return &harness{driver: d, guest: channel.NewGuest(d)}
}

// agentOf gives the Machine a fake agent on the Guest Agent's port.
func (h *harness) agentOf(t *testing.T, id machine.MachineID) *state {
	t.Helper()
	st := &state{fail: map[string]error{}, execStatus: guestv1alpha1.ExecStatus_EXEC_STATUS_STARTED}
	h.driver.ServeGuest(id, machine.GuestAgentPort, st.serve)
	t.Cleanup(func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, s := range st.servers {
			s.Stop()
		}
	})
	return st
}

// distinct returns n bytes in which no run repeats at a shift under n, so a
// stream that repeats or skips a run does not read the same.
func distinct(n int) string {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return string(b)
}

func within[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

var (
	addr4 = netip.MustParseAddr("10.0.0.5")
	addr6 = netip.MustParseAddr("fd00::5")
	gw4   = netip.MustParseAddr("10.0.255.254")
	gw6   = netip.MustParseAddr("fe80::1")
	res4  = netip.MustParseAddr("10.0.255.253")
	res6  = netip.MustParseAddr("fd00::53")
)

// Boot carries the whole boot description, the Machine's network with it, and
// goes to the Machine it names.
func TestBoot_SendsTheWholeDescriptionToTheMachineNamed(t *testing.T) {
	h := newHarness(t)
	a, b := h.agentOf(t, "a"), h.agentOf(t, "b")
	boot := machine.GuestBoot{
		ImageUUID:    "11111111-1111-1111-1111-111111111111",
		RootDiskUUID: "22222222-2222-2222-2222-222222222222",
		Run:          machine.ExecSpec{Command: []string{"/bin/app", "--x"}, Env: map[string]string{"K": "V"}, WorkingDir: "/srv"},
		Network:      machine.GuestNetwork{Addresses: []netip.Addr{addr4, addr6}, Gateways: []netip.Addr{gw4, gw6}, Resolvers: []netip.Addr{res4, res6}},
	}

	require.NoError(t, h.guest.Boot(context.Background(), "a", boot))

	req := a.boot
	require.NotNil(t, req)
	assert.Equal(t, boot.ImageUUID, req.GetImageUuid())
	assert.Equal(t, boot.RootDiskUUID, req.GetRootDiskUuid())
	assert.Equal(t, boot.Run.Command, req.GetRun().GetArgv())
	assert.Equal(t, boot.Run.Env, req.GetRun().GetEnv())
	assert.Equal(t, "/srv", req.GetRun().GetWorkingDir())
	assert.Equal(t, "10.0.0.5/32", req.GetNetwork().GetIpv4().GetAddress(), "the address with its prefix length")
	assert.Equal(t, "10.0.255.254", req.GetNetwork().GetIpv4().GetGateway())
	assert.Equal(t, "fd00::5/128", req.GetNetwork().GetIpv6().GetAddress())
	assert.Equal(t, "fe80::1", req.GetNetwork().GetIpv6().GetGateway())
	assert.Equal(t, []string{"10.0.255.253", "fd00::53"}, req.GetNetwork().GetResolvers())
	assert.Nil(t, b.boot, "the other Machine was not told")

	// Control: a node that serves one family tells the guest of that one.
	boot.Network = machine.GuestNetwork{Addresses: []netip.Addr{addr4}, Gateways: []netip.Addr{gw4}, Resolvers: []netip.Addr{res4}}
	require.NoError(t, h.guest.Boot(context.Background(), "b", boot))
	assert.Equal(t, "10.0.0.5/32", b.boot.GetNetwork().GetIpv4().GetAddress())
	assert.Nil(t, b.boot.GetNetwork().GetIpv6(), "a family not served gets nothing")
}

// CatchUp carries the restore, the entropy, the host's time and the whole
// network: an empty one would remove the Machine's addresses.
func TestCatchUp_SendsTheWholeRequest(t *testing.T) {
	h := newHarness(t)
	a := h.agentOf(t, "a")
	when := time.Unix(1_700_000_000, 123)
	err := h.guest.CatchUp(context.Background(), "a", machine.CatchUp{
		RestoreID: "r-1",
		Entropy:   []byte("0123456789abcdef0123456789abcdef"),
		HostTime:  when,
		Network:   machine.GuestNetwork{Addresses: []netip.Addr{addr4, addr6}, Gateways: []netip.Addr{gw4, gw6}, Resolvers: []netip.Addr{res4}},
	})
	require.NoError(t, err)

	req := a.catchUp
	require.NotNil(t, req)
	assert.Equal(t, "r-1", req.GetRestoreId())
	assert.Equal(t, []byte("0123456789abcdef0123456789abcdef"), req.GetEntropy())
	assert.Equal(t, when.UnixNano(), req.GetHostUnixNanos())
	assert.Equal(t, "10.0.0.5/32", req.GetNetwork().GetIpv4().GetAddress())
	assert.Equal(t, "10.0.255.254", req.GetNetwork().GetIpv4().GetGateway())
	assert.Equal(t, "fd00::5/128", req.GetNetwork().GetIpv6().GetAddress())
	assert.Equal(t, "fe80::1", req.GetNetwork().GetIpv6().GetGateway())
	assert.Equal(t, []string{"10.0.255.253"}, req.GetNetwork().GetResolvers())
}

// Each status the agent sends is its own record status; a status the daemon
// does not know is an error and not a guess.
func TestExec_MapsEachStatus(t *testing.T) {
	tests := []struct {
		sent guestv1alpha1.ExecStatus
		want machine.ExecStatus
		bad  bool
	}{
		{guestv1alpha1.ExecStatus_EXEC_STATUS_NOT_STARTED, machine.ExecNotStarted, false},
		{guestv1alpha1.ExecStatus_EXEC_STATUS_STARTED, machine.ExecStarted, false},
		{guestv1alpha1.ExecStatus_EXEC_STATUS_EXITED, machine.ExecExited, false},
		{guestv1alpha1.ExecStatus_EXEC_STATUS_CUT_SHORT, machine.ExecCutShort, false},
		{guestv1alpha1.ExecStatus_EXEC_STATUS_UNSPECIFIED, 0, true},
		{guestv1alpha1.ExecStatus(99), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.sent.String(), func(t *testing.T) {
			h := newHarness(t)
			a := h.agentOf(t, "m")
			a.execStatus = tt.sent
			rec, err := h.guest.Exec(context.Background(), "m", "x", machine.ExecSpec{Command: []string{"ls", "-l"}, Env: map[string]string{"A": "b"}, WorkingDir: "/w"})
			if tt.bad {
				require.Error(t, err)
				assert.NotErrorIs(t, err, machine.ErrGuestUnreachable)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, machine.ExecRecord{ID: "x", Status: tt.want, Reason: "why", ExitCode: 3, TermSignal: 9}, rec)
			assert.Equal(t, "x", a.exec.GetExecId())
			assert.Equal(t, []string{"ls", "-l"}, a.exec.GetArgv())
			assert.Equal(t, map[string]string{"A": "b"}, a.exec.GetEnv())
			assert.Equal(t, "/w", a.exec.GetWorkingDir())
		})
	}
}

// Output chunks keep their stream and offset, and the stream ends with the
// record.
func TestReadExecOutput_MapsStreamsAndTheRecord(t *testing.T) {
	for _, tt := range []struct {
		sent guestv1alpha1.OutputStream
		want machine.OutputStream
		bad  bool
	}{
		{guestv1alpha1.OutputStream_OUTPUT_STREAM_STDOUT, machine.OutputStdout, false},
		{guestv1alpha1.OutputStream_OUTPUT_STREAM_STDERR, machine.OutputStderr, false},
		{guestv1alpha1.OutputStream_OUTPUT_STREAM_UNSPECIFIED, 0, true},
	} {
		t.Run(tt.sent.String(), func(t *testing.T) {
			h := newHarness(t)
			a := h.agentOf(t, "m")
			a.output, a.outStream = []byte(strings.Repeat("x", 150)), tt.sent
			var got []machine.OutputChunk
			rec, err := h.guest.ReadExecOutput(context.Background(), "m", "x", 100, func(c machine.OutputChunk) error {
				got = append(got, c)
				return nil
			})
			if tt.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []machine.OutputChunk{{Stream: tt.want, Offset: 100, Data: []byte(strings.Repeat("x", 50))}}, got)
			assert.Equal(t, machine.ExecRecord{ID: "x", Status: machine.ExecExited, ExitCode: 7}, rec)
			assert.Equal(t, []uint64{100}, a.outAfter)
		})
	}
}

func TestPatchFile_MapsEachOutcome(t *testing.T) {
	for _, tt := range []struct {
		sent guestv1alpha1.PatchOutcome
		want machine.PatchOutcome
		bad  bool
	}{
		{guestv1alpha1.PatchOutcome_PATCH_OUTCOME_APPLIED, machine.PatchApplied, false},
		{guestv1alpha1.PatchOutcome_PATCH_OUTCOME_ALREADY_APPLIED, machine.PatchAlreadyApplied, false},
		{guestv1alpha1.PatchOutcome_PATCH_OUTCOME_UNSPECIFIED, 0, true},
	} {
		t.Run(tt.sent.String(), func(t *testing.T) {
			h := newHarness(t)
			a := h.agentOf(t, "m")
			a.patchOut = tt.sent
			got, err := h.guest.PatchFile(context.Background(), "m", "/f", "sha256:b", "sha256:e", []byte("p"))
			if tt.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, "/f", a.patch.GetPath())
			assert.Equal(t, "sha256:b", a.patch.GetBaseDigest())
			assert.Equal(t, "sha256:e", a.patch.GetExpectedDigest())
			assert.Equal(t, []byte("p"), a.patch.GetPatch())
		})
	}
}

// Each status of the agent becomes the error the port promises, by what the
// call was doing. The rows whose want is nil are controls: a status that must
// not become that sentinel.
func TestErrors_AreMappedToThePortsErrors(t *testing.T) {
	ctx := context.Background()
	st := func(c codes.Code, msg string) error { return status.Error(c, msg) }
	detailed := func(c codes.Code, msg string, d protoadapt.MessageV1) error {
		withDetail, err := status.New(c, msg).WithDetails(d)
		require.NoError(t, err)
		return withDetail.Err()
	}
	calls := map[string]func(h *harness) error{
		"Health": func(h *harness) error { return h.guest.Health(ctx, "m") },
		"Boot": func(h *harness) error {
			return h.guest.Boot(ctx, "m", machine.GuestBoot{})
		},
		"CatchUp": func(h *harness) error { return h.guest.CatchUp(ctx, "m", machine.CatchUp{}) },
		"Exec": func(h *harness) error {
			_, err := h.guest.Exec(ctx, "m", "x", machine.ExecSpec{})
			return err
		},
		"ReadExecOutput": func(h *harness) error {
			_, err := h.guest.ReadExecOutput(ctx, "m", "x", 0, func(machine.OutputChunk) error { return nil })
			return err
		},
		"ReadFile": func(h *harness) error {
			return h.guest.ReadFile(ctx, "m", "/f", func([]byte) error { return nil })
		},
		"WriteFile": func(h *harness) error { return h.guest.WriteFile(ctx, "m", "/f", strings.NewReader("c")) },
		"PatchFile": func(h *harness) error {
			_, err := h.guest.PatchFile(ctx, "m", "/f", "b", "e", nil)
			return err
		},
		"SearchFiles": func(h *harness) error {
			_, err := h.guest.SearchFiles(ctx, "m", "/", "*")
			return err
		},
	}
	sentinels := []error{machine.ErrExecAbsent, machine.ErrFileAbsent, machine.ErrNotRegular, machine.ErrPatchPrecondition, machine.ErrGuestUnreachable}
	tests := []struct {
		name   string
		call   string
		answer error
		want   error // a sentinel, or nil when none may match
		check  func(t *testing.T, err error)
	}{
		{name: "an exec that is not held", call: "Exec", answer: st(codes.NotFound, "not found: exec x"), want: machine.ErrExecAbsent},
		{name: "output of an exec that is not held", call: "ReadExecOutput", answer: st(codes.NotFound, "not found: exec x"), want: machine.ErrExecAbsent},
		{name: "a path that names nothing, read", call: "ReadFile", answer: st(codes.NotFound, "not found: /f"), want: machine.ErrFileAbsent},
		{name: "a path that names nothing, patched", call: "PatchFile", answer: st(codes.NotFound, "not found: /f"), want: machine.ErrFileAbsent},
		{name: "a path that names nothing, searched", call: "SearchFiles", answer: st(codes.NotFound, "not found: /"), want: machine.ErrFileAbsent},
		// WrongFileKind. The controls: the same code with no detail maps by its
		// code alone, even where the message says what a detail would, and a
		// detail naming another kind of file is not a file that is not regular.
		{name: "a file that is not a regular file, read", call: "ReadFile", answer: detailed(codes.InvalidArgument, "x", &guestv1alpha1.WrongFileKind{Path: "/f", Expected: guestv1alpha1.EntryKind_ENTRY_KIND_FILE}), want: machine.ErrNotRegular},
		{name: "a file that is not a regular file, written", call: "WriteFile", answer: detailed(codes.InvalidArgument, "x", &guestv1alpha1.WrongFileKind{Path: "/f", Expected: guestv1alpha1.EntryKind_ENTRY_KIND_FILE}), want: machine.ErrNotRegular},
		{name: "a file where a directory is needed", call: "SearchFiles", answer: detailed(codes.InvalidArgument, "x", &guestv1alpha1.WrongFileKind{Path: "/f", Expected: guestv1alpha1.EntryKind_ENTRY_KIND_DIRECTORY})},
		{name: "the same code with no detail", call: "ReadFile", answer: st(codes.InvalidArgument, "invalid request: /f is not a regular file")},
		{name: "invalid input is not a missing file", call: "ReadFile", answer: st(codes.InvalidArgument, "invalid request: path f is not absolute")},
		// Patch: a precondition with no detail is the digests' by its code and
		// call, whatever its message says; one with NotBooted is not.
		{name: "a patch whose file is at neither digest", call: "PatchFile", answer: st(codes.FailedPrecondition, "x"), want: machine.ErrPatchPrecondition},
		{name: "a patch precondition whose message talks of booting", call: "PatchFile", answer: st(codes.FailedPrecondition, "the guest has not booted"), want: machine.ErrPatchPrecondition},
		{name: "a patch to a guest that has not booted", call: "PatchFile", answer: detailed(codes.FailedPrecondition, "x", &guestv1alpha1.NotBooted{State: guestv1alpha1.BootState_BOOT_STATE_PENDING})},
		{name: "a patch to a guest that failed to boot", call: "PatchFile", answer: detailed(codes.FailedPrecondition, "x", &guestv1alpha1.NotBooted{State: guestv1alpha1.BootState_BOOT_STATE_FAILED})},
		{name: "an exec to a guest that has not booted", call: "Exec", answer: detailed(codes.FailedPrecondition, "x", &guestv1alpha1.NotBooted{State: guestv1alpha1.BootState_BOOT_STATE_PENDING})},
		{name: "a connection the agent lost", call: "Health", answer: st(codes.Unavailable, "transport is closing"), want: machine.ErrGuestUnreachable},
		// ExecOutputGone, with the same code and no detail as the control.
		{
			name: "output the agent no longer holds", call: "ReadExecOutput",
			answer: detailed(codes.OutOfRange, "x", &guestv1alpha1.ExecOutputGone{EarliestOffset: 4096}),
			check: func(t *testing.T, err error) {
				var gone *machine.OutputGoneError
				require.ErrorAs(t, err, &gone)
				assert.Equal(t, uint64(4096), gone.Earliest)
			},
		},
		{name: "out of range with no detail, whatever the message says", call: "ReadExecOutput", answer: st(codes.OutOfRange, "output before offset 4096 is no longer held; the earliest held offset is 4096")},
		// BootFailed, with the same code and a message naming a step as controls.
		{
			name: "a failed boot step", call: "Boot",
			answer: detailed(codes.Internal, "x", &guestv1alpha1.BootFailed{Step: guestv1alpha1.BootStep_BOOT_STEP_MOUNT_IMAGE, Detail: "no device carries the UUID"}),
			check: func(t *testing.T, err error) {
				var step *machine.BootStepError
				require.ErrorAs(t, err, &step)
				assert.Equal(t, "mount the image", step.Step)
				assert.Equal(t, "no device carries the UUID", step.Detail)
			},
		},
		{
			name: "a repeat of a boot that failed", call: "Boot",
			answer: detailed(codes.FailedPrecondition, "x", &guestv1alpha1.BootFailed{Step: guestv1alpha1.BootStep_BOOT_STEP_OVERLAY, Detail: "busy"}),
			check: func(t *testing.T, err error) {
				var step *machine.BootStepError
				require.ErrorAs(t, err, &step)
				assert.Equal(t, "overlay the root disk", step.Step)
			},
		},
		{name: "an internal failure with no detail, whatever the message says", call: "Boot", answer: st(codes.Internal, "boot step mount the image: no device carries the UUID")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			a := h.agentOf(t, "m")
			a.fail[tt.call] = tt.answer
			err := calls[tt.call](h)
			require.Error(t, err)
			if tt.check != nil {
				tt.check(t, err)
				return
			}
			for _, s := range sentinels {
				if s == tt.want {
					assert.ErrorIs(t, err, s)
				} else {
					assert.NotErrorIs(t, err, s)
				}
			}
			var step *machine.BootStepError
			assert.False(t, errors.As(err, &step), "no boot step is named")
			var gone *machine.OutputGoneError
			assert.False(t, errors.As(err, &gone))
		})
	}

	t.Run("control: with nothing failing every call succeeds", func(t *testing.T) {
		for name, call := range calls {
			h := newHarness(t)
			a := h.agentOf(t, "m")
			a.patchOut = guestv1alpha1.PatchOutcome_PATCH_OUTCOME_APPLIED
			a.execStatus = guestv1alpha1.ExecStatus_EXEC_STATUS_STARTED
			assert.NoError(t, call(h), name)
		}
	})
}

// A Machine nothing serves, or whose connection cannot be opened, is a guest
// that is not reachable, for every call.
func TestUnreachable_WhenTheDriverCannotOpenAStream(t *testing.T) {
	ctx := context.Background()

	t.Run("a Machine whose guest port nothing serves", func(t *testing.T) {
		h := newHarness(t)
		h.driver.ServeGuest("m", machine.GuestAgentPort+1, func(c net.Conn) { _ = c.Close() })
		require.ErrorIs(t, h.guest.Health(ctx, "m"), machine.ErrGuestUnreachable)
		require.ErrorIs(t, h.guest.Boot(ctx, "m", machine.GuestBoot{}), machine.ErrGuestUnreachable)
		_, err := h.guest.Exec(ctx, "m", "x", machine.ExecSpec{})
		require.ErrorIs(t, err, machine.ErrGuestUnreachable)
		assert.Equal(t, 6, h.driver.Calls("guest-conn"), "the Driver was asked, and once more for each call's repeat")
	})

	t.Run("a failure of the Driver", func(t *testing.T) {
		h := newHarness(t)
		h.agentOf(t, "m")
		h.driver.FailAt("guest-conn", errBoom)
		require.ErrorIs(t, h.guest.Health(ctx, "m"), machine.ErrGuestUnreachable)

		// Control: the same call reaches the agent once the Driver heals.
		h.driver.Heal("guest-conn")
		require.NoError(t, h.guest.Health(ctx, "m"))
	})
}

// Every call goes to the Machine it names, on the Guest Agent's port.
func TestCalls_GoToTheMachineNamed(t *testing.T) {
	h := newHarness(t)
	a, b := h.agentOf(t, "a"), h.agentOf(t, "b")

	_, err := h.guest.Exec(context.Background(), "a", "only-a", machine.ExecSpec{Command: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, "only-a", a.exec.GetExecId())
	assert.Nil(t, b.exec)
	_, err = h.guest.Exec(context.Background(), "b", "only-b", machine.ExecSpec{Command: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, "only-b", b.exec.GetExecId())
	assert.Equal(t, "only-a", a.exec.GetExecId())
}

// A connection that breaks before the call is answered is replaced by another,
// and the call repeats once.
func TestCalls_RepeatOnANewConnectionAfterAReset(t *testing.T) {
	ctx := context.Background()

	t.Run("the first connection is reset", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		a.dropOnConnect = func(n int) bool { return n == 1 }

		_, err := h.guest.Exec(ctx, "m", "x", machine.ExecSpec{Command: []string{"x"}})
		require.NoError(t, err)
		assert.Equal(t, "x", a.exec.GetExecId(), "the repeat reached the agent")
		assert.Equal(t, 2, a.connections())
		assert.Equal(t, 2, h.driver.Calls("guest-conn"))
	})

	t.Run("control: with no reset one connection serves the call", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		_, err := h.guest.Exec(ctx, "m", "x", machine.ExecSpec{Command: []string{"x"}})
		require.NoError(t, err)
		assert.Equal(t, 1, a.connections())
	})

	t.Run("a guest that keeps resetting is not retried for ever", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		a.dropOnConnect = func(int) bool { return true }

		require.ErrorIs(t, h.guest.Health(ctx, "m"), machine.ErrGuestUnreachable)
		assert.Equal(t, 2, a.connections(), "one repeat")
	})

	t.Run("a refusal is not a reset and is not repeated", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		a.fail["Exec"] = status.Error(codes.InvalidArgument, "invalid request: argv is empty")
		_, err := h.guest.Exec(ctx, "m", "x", machine.ExecSpec{})
		require.Error(t, err)
		assert.Equal(t, 1, a.connections())
	})
}

// A stream cut by a reset resumes after the last byte passed on, so the caller
// sees every byte once.
func TestReadExecOutput_ResumesAfterTheLastByteAfterAReset(t *testing.T) {
	h := newHarness(t)
	a := h.agentOf(t, "m")
	want := distinct(1000)
	a.output, a.outStream, a.holdAfter = []byte(want), guestv1alpha1.OutputStream_OUTPUT_STREAM_STDOUT, 4

	var got []byte
	half := make(chan struct{})
	type result struct {
		rec machine.ExecRecord
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec, err := h.guest.ReadExecOutput(context.Background(), "m", "x", 0, func(c machine.OutputChunk) error {
			assert.Equal(t, uint64(len(got)), c.Offset, "no gap and no repeat")
			got = append(got, c.Data...)
			if len(got) == 400 {
				close(half)
			}
			return nil
		})
		done <- result{rec, err}
	}()
	within(t, half)
	a.closeConn(1)

	r := within(t, done)
	require.NoError(t, r.err)
	assert.Equal(t, want, string(got))
	assert.Equal(t, machine.ExecExited, r.rec.Status)
	assert.Equal(t, []uint64{0, 400}, a.outAfter, "the second read began where the first was cut")
}

// A file read cut by a reset is read again and the bytes already passed on
// are skipped, whatever size the chunks of the new connection are.
func TestReadFile_ResumesAfterAReset(t *testing.T) {
	h := newHarness(t)
	a := h.agentOf(t, "m")
	content := distinct(1000)
	a.file, a.holdAfter = []byte(content), 4
	a.fileChunk = func(n int) int {
		if n == 1 {
			return 100
		}
		return 150 // 400 falls inside the third chunk of the new connection
	}

	var got []byte
	half := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- h.guest.ReadFile(context.Background(), "m", "/f", func(c []byte) error {
			got = append(got, c...)
			if len(got) == 400 {
				close(half)
			}
			return nil
		})
	}()
	within(t, half)
	a.closeConn(1)

	require.NoError(t, within(t, done))
	assert.Equal(t, content, string(got), "every byte once, in order")
	assert.Equal(t, 2, a.connections())
}

// A write sends the digest of what it sent, and a reset after its content was
// read is not repeated, since the reader cannot be read again.
func TestWriteFile_SendsTheDigestAndIsNotRepeatedOnceRead(t *testing.T) {
	ctx := context.Background()
	content := strings.Repeat("0123456789", 5000)
	sum := sha256.Sum256([]byte(content))

	t.Run("the digest of the whole content", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		require.NoError(t, h.guest.WriteFile(ctx, "m", "/f", strings.NewReader(content)))
		assert.Equal(t, content, string(a.written))
		assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), a.digest)
	})

	t.Run("a reset before any content is read is repeated", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		a.dropOnConnect = func(n int) bool { return n == 1 }
		require.NoError(t, h.guest.WriteFile(ctx, "m", "/f", strings.NewReader(content)))
		assert.Equal(t, content, string(a.written))
	})

	t.Run("a reset after content was read is reported, not repeated", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		a.holdWrite = true
		done := make(chan error, 1)
		go func() { done <- h.guest.WriteFile(ctx, "m", "/f", strings.NewReader(content)) }()
		require.Eventually(t, func() bool { return a.connections() == 1 }, 5*time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond) // the agent has the first chunk and holds
		a.closeConn(1)

		require.ErrorIs(t, within(t, done), machine.ErrGuestUnreachable)
		assert.Equal(t, 1, h.driver.Calls("guest-conn"), "the content was not sent again")
		assert.Empty(t, a.written, "the agent kept the old file")
	})
}

func TestSearchFiles_ReturnsThePaths(t *testing.T) {
	h := newHarness(t)
	a := h.agentOf(t, "m")
	a.paths = []string{"/a/x", "/b/x"}
	got, err := h.guest.SearchFiles(context.Background(), "m", "/", "**/x")
	require.NoError(t, err)
	assert.Equal(t, []string{"/a/x", "/b/x"}, got)
}

// The adapter's own time limit would cut a Boot the lifecycle bounds, so the
// only bound on a call is the caller's context.
func TestCalls_AreBoundedOnlyByTheCallersContext(t *testing.T) {
	t.Run("a slow answer is waited for", func(t *testing.T) {
		h := newHarness(t)
		a := h.agentOf(t, "m")
		a.fail["Boot"] = nil
		h.driver.ServeGuest("m", machine.GuestAgentPort, func(c net.Conn) {
			time.Sleep(300 * time.Millisecond)
			a.serve(c)
		})
		require.NoError(t, h.guest.Boot(context.Background(), "m", machine.GuestBoot{}))
	})

	t.Run("the caller's deadline ends the call", func(t *testing.T) {
		h := newHarness(t)
		h.driver.ServeGuest("m", machine.GuestAgentPort, func(c net.Conn) { <-time.After(5 * time.Second); _ = c.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := h.guest.Boot(ctx, "m", machine.GuestBoot{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, machine.ErrGuestUnreachable)
	})
}
