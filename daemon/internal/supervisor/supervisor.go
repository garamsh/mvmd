// Package supervisor is the mvmd-supervisor process: the init of a PID
// namespace that counts one Machine's fencing expiry, starts the Machine's VMM
// inside the namespace and ends the namespace at the expiry or when the VMM
// ends (ADR 0003). It holds nothing of the inside but the VMM's serial bytes in
// transit, which it serves as a console and interprets none of (ADR 0020, ADR
// 0025).
package supervisor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// APIVersion is the supervisor API this release speaks.
const APIVersion = "v1alpha1"

// Config is what mvmd gives a supervisor when it starts it.
type Config struct {
	// Socket is the Unix socket the supervisor listens on, in its Machine's
	// state directory.
	Socket string
	// Machine is the identifier of the Machine the supervisor serves.
	Machine string
	// Expiry is the absolute CLOCK_BOOTTIME value, in nanoseconds since boot,
	// at which the supervisor ends its namespace.
	Expiry int64
}

// check refuses a start with no expiry, with one that has passed, or outside
// the init of a PID namespace.
func (c Config) check() error {
	switch {
	case c.Socket == "":
		return errors.New("no socket was given")
	case c.Machine == "":
		return errors.New("no Machine was given")
	case c.Expiry <= 0:
		return errors.New("no expiry was given")
	case os.Getpid() != 1:
		return errors.New("the supervisor is not the init of a PID namespace")
	}
	now, err := boottime()
	if err != nil {
		return err
	}
	if c.Expiry <= now {
		return fmt.Errorf("the expiry %d has passed (the boot clock reads %d)", c.Expiry, now)
	}
	return nil
}

type server struct {
	// Unsafe, not Unimplemented: an rpc added to the proto without a method
	// here fails the build instead of answering codes.Unimplemented.
	pb.UnsafeSupervisorServiceServer
	cfg   Config
	timer *timer

	mu      sync.Mutex
	expiry  int64
	started bool
	vmm     int
	console *Console
}

// Run counts the expiry and serves the socket until the process exits, which
// it does when the expiry passes or the VMM ends. It returns only on a failure
// before either.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.check(); err != nil {
		return err
	}
	if err := lockMemory(); err != nil {
		return err
	}
	t, err := newTimer()
	if err != nil {
		return err
	}
	if err := t.arm(cfg.Expiry); err != nil {
		return err
	}
	ready := make(chan error, 1)
	go t.enforce(ready)
	if err := <-ready; err != nil {
		return err
	}

	_ = os.Remove(cfg.Socket)
	l, err := unixsock.Listen(cfg.Socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Socket, err)
	}
	if err := os.Chmod(cfg.Socket, 0o600); err != nil {
		return fmt.Errorf("restrict %s: %w", cfg.Socket, err)
	}
	s := &server{cfg: cfg, timer: t, expiry: cfg.Expiry}
	g := grpc.NewServer()
	pb.RegisterSupervisorServiceServer(g, s)
	go s.reap(ctx)
	go func() {
		<-ctx.Done()
		g.Stop()
	}()
	return g.Serve(l)
}

// reap collects every child of the init, since processes of the namespace are
// reparented to it, and ends the supervisor when the VMM ends: a Machine whose
// VMM is gone has nothing left to supervise.
func (s *server) reap(ctx context.Context) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	for {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			s.mu.Lock()
			ended := s.started && pid == s.vmm
			console := s.console
			s.mu.Unlock()
			if ended {
				// What the VMM wrote before it ended is still in the terminal: the
				// log is complete once the console has read it to its end.
				console.Wait()
				os.Exit(0)
			}
		}
		select {
		case <-sig:
		case <-ctx.Done():
			return
		}
	}
}

func (s *server) Start(_ context.Context, req *pb.StartRequest) (*pb.StartResponse, error) {
	p := req.GetProcess()
	if p.GetExecutable() == "" {
		return nil, status.Error(codes.InvalidArgument, "the process has no executable")
	}
	if req.GetUid() == 0 || req.GetGid() == 0 {
		return nil, status.Error(codes.InvalidArgument, "the Machine's user and group must not be root (uid and gid nonzero)")
	}
	if err := checkRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	lim, err := limitsFor(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil, status.Error(codes.FailedPrecondition, "a process was already started")
	}
	now, err := boottime()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if s.expiry <= now {
		return nil, status.Error(codes.FailedPrecondition, "the expiry has passed")
	}
	pid, console, err := launch(s.cfg.Socket, req, lim)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "start the process: %v", err)
	}
	s.started, s.vmm, s.console = true, pid, console
	return &pb.StartResponse{}, nil
}

func (s *server) SetExpiry(_ context.Context, req *pb.SetExpiryRequest) (*pb.SetExpiryResponse, error) {
	if req.GetExpiryBoottimeNs() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "no expiry was given")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.timer.arm(req.GetExpiryBoottimeNs()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.expiry = req.GetExpiryBoottimeNs()
	return &pb.SetExpiryResponse{ExpiryBoottimeNs: s.expiry}, nil
}

func (s *server) Inspect(context.Context, *pb.InspectRequest) (*pb.InspectResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	running := false
	if s.started {
		running = unix.Kill(s.vmm, 0) == nil
	}
	resp := &pb.InspectResponse{
		MachineId:        s.cfg.Machine,
		ExpiryBoottimeNs: s.expiry,
		ProcessRunning:   running,
		ApiVersion:       APIVersion,
	}
	if running {
		resp.VmmPid = int32(s.vmm)
	}
	return resp, nil
}

// Main is the binary: it reads the flags mvmd starts it with and runs until
// the namespace ends. It returns the process's exit code.
func Main(args []string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == confineCommand {
		return confineMain(args[1:])
	}
	fs := flag.NewFlagSet("mvmd-supervisor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg Config
	fs.StringVar(&cfg.Socket, "socket", "", "the Unix socket to listen on")
	fs.StringVar(&cfg.Machine, "machine", "", "the identifier of the Machine served")
	fs.Int64Var(&cfg.Expiry, "expiry-boottime-ns", 0, "the absolute CLOCK_BOOTTIME expiry in nanoseconds")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := Run(context.Background(), cfg); err != nil {
		fmt.Fprintln(stderr, "mvmd-supervisor:", err)
		return 1
	}
	return 0
}
