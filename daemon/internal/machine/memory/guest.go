package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Guest is an in-memory machine.Guest: one guest per Machine, holding files
// and commands. It does not model a VMM, so it answers whether or not the
// Machine runs; the lifecycle is what refuses a call. Its steps are "health",
// "boot", "catch-up", "exec", "read-exec-output", "signal",
// "read-file", "write-file", "patch-file", "list-files", "search-files", "terminate", "find-disk",
// "release-disk" and "online-vcpus"; a failing step
// answers machine.ErrGuestUnreachable unless told another error.
type Guest struct {
	Failures

	mu        sync.Mutex
	files     map[machine.MachineID]map[string][]byte
	special   map[machine.MachineID]map[string]bool
	execs     map[machine.ExecID]*guestExec
	signals   map[machine.ExecID][]int32
	catchUps  map[machine.MachineID][]machine.CatchUp
	boots     map[machine.MachineID][]machine.GuestBoot
	refused   map[machine.ExecID]string
	loseExec  bool
	retention int
	ends      func(machine.MachineID)
	refuse    error
	terms     map[machine.MachineID]int
	found     map[machine.MachineID]map[machine.DiskID]bool
	released  map[machine.MachineID]map[machine.DiskID]bool
	held      map[machine.MachineID]map[machine.DiskID]bool
	present   map[machine.MachineID]uint32
	online    map[machine.MachineID]uint32
}

type guestExec struct {
	record machine.ExecRecord
	output []machine.OutputChunk
	next   uint64
	held   int
}

// NewGuest returns a Guest with no files and no commands, holding the newest
// retention bytes of each command's output, all of it when retention is zero.
func NewGuest(retention int) *Guest {
	return &Guest{
		files:     make(map[machine.MachineID]map[string][]byte),
		special:   make(map[machine.MachineID]map[string]bool),
		execs:     make(map[machine.ExecID]*guestExec),
		signals:   make(map[machine.ExecID][]int32),
		catchUps:  make(map[machine.MachineID][]machine.CatchUp),
		boots:     make(map[machine.MachineID][]machine.GuestBoot),
		refused:   make(map[machine.ExecID]string),
		terms:     make(map[machine.MachineID]int),
		found:     make(map[machine.MachineID]map[machine.DiskID]bool),
		released:  make(map[machine.MachineID]map[machine.DiskID]bool),
		held:      make(map[machine.MachineID]map[machine.DiskID]bool),
		present:   make(map[machine.MachineID]uint32),
		online:    make(map[machine.MachineID]uint32),
		retention: retention,
	}
}

// Retain makes the guest hold the newest n bytes of each command's output from
// now on, all of it when n is zero.
func (g *Guest) Retain(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retention = n
}

// Digest is the digest of content as a patch names it.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// enter counts the step and turns its injected failure into the unreachable
// error, so the lifecycle's mapping of a guest that cannot be reached is the
// one under test.
func (g *Guest) enter(step string) error {
	if err := g.Enter(step); err != nil {
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	return nil
}

// Health implements machine.Guest.
func (g *Guest) Health(_ context.Context, _ machine.MachineID) error { return g.enter("health") }

// Boot implements machine.Guest. A step failed with a *machine.BootStepError
// is the agent answering that a boot step failed, and is returned as it is.
func (g *Guest) Boot(ctx context.Context, id machine.MachineID, boot machine.GuestBoot) error {
	if err := g.Enter("boot"); err != nil {
		var step *machine.BootStepError
		if errors.As(err, &step) {
			return err
		}
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	// A call whose context has ended did not reach the guest, as the real
	// channel's would not.
	if err := ctx.Err(); err != nil {
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	g.mu.Lock()
	g.boots[id] = append(g.boots[id], boot)
	g.mu.Unlock()
	return nil
}

// Boots returns the boot descriptions the guest of the Machine was sent, in
// order.
func (g *Guest) Boots(id machine.MachineID) []machine.GuestBoot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.boots[id])
}

// CatchUp implements machine.Guest.
func (g *Guest) CatchUp(_ context.Context, id machine.MachineID, catchUp machine.CatchUp) error {
	if err := g.enter("catch-up"); err != nil {
		return err
	}
	g.mu.Lock()
	g.catchUps[id] = append(g.catchUps[id], catchUp)
	g.mu.Unlock()
	return nil
}

// CatchUps returns what the guest of the Machine was told, in order.
func (g *Guest) CatchUps(id machine.MachineID) []machine.CatchUp {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]machine.CatchUp(nil), g.catchUps[id]...)
}

// LoseExecAnswer makes the next Exec start its command and then fail as a call
// that did not get an answer: the command runs and the caller cannot know.
func (g *Guest) LoseExecAnswer() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loseExec = true
}

// RefuseExec makes Exec answer with a record that says the command did not
// start, for the identifier.
func (g *Guest) RefuseExec(exec machine.ExecID, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused[exec] = reason
}

// Exec implements machine.Guest: the command starts and runs until Finish.
func (g *Guest) Exec(_ context.Context, _ machine.MachineID, exec machine.ExecID, _ machine.ExecSpec) (machine.ExecRecord, error) {
	if err := g.enter("exec"); err != nil {
		return machine.ExecRecord{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if reason, ok := g.refused[exec]; ok {
		return machine.ExecRecord{ID: exec, Status: machine.ExecNotStarted, Reason: reason}, nil
	}
	e, ok := g.execs[exec]
	if !ok {
		e = &guestExec{record: machine.ExecRecord{ID: exec, Status: machine.ExecStarted}}
		g.execs[exec] = e
	}
	if g.loseExec {
		g.loseExec = false
		return machine.ExecRecord{}, machine.ErrGuestUnreachable
	}
	return e.record, nil
}

// Started returns how many commands the guests were started with the
// identifier: one or none, since a repeat starts nothing.
func (g *Guest) Started(exec machine.ExecID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.execs[exec]; ok {
		return 1
	}
	return 0
}

// Emit makes the command print the data on the stream, and drops the oldest
// chunks beyond the retention.
func (g *Guest) Emit(exec machine.ExecID, stream machine.OutputStream, data string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.execs[exec]
	e.output = append(e.output, machine.OutputChunk{Stream: stream, Offset: e.next, Data: []byte(data)})
	e.next += uint64(len(data))
	e.held += len(data)
	for g.retention > 0 && e.held > g.retention && len(e.output) > 1 {
		e.held -= len(e.output[0].Data)
		e.output = e.output[1:]
	}
}

// Finish ends the command with the exit code.
func (g *Guest) Finish(exec machine.ExecID, exitCode int32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := &g.execs[exec].record
	r.Status, r.ExitCode = machine.ExecExited, exitCode
}

// Signal implements machine.Guest: a command that runs takes the signal, which
// Signals reports, and one that has ended refuses it.
func (g *Guest) Signal(_ context.Context, _ machine.MachineID, exec machine.ExecID, signal int32) error {
	if err := g.enter("signal"); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.execs[exec]
	switch {
	case !ok:
		return machine.ErrExecAbsent
	case e.record.Status != machine.ExecStarted:
		return machine.ErrExecNotRunning
	}
	g.signals[exec] = append(g.signals[exec], signal)
	return nil
}

// Signals returns the signals the command took, in order.
func (g *Guest) Signals(exec machine.ExecID) []int32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.signals[exec])
}

// ReadExecOutput implements machine.Guest.
func (g *Guest) ReadExecOutput(_ context.Context, _ machine.MachineID, exec machine.ExecID, after uint64, send func(machine.OutputChunk) error) (machine.ExecRecord, error) {
	if err := g.enter("read-exec-output"); err != nil {
		return machine.ExecRecord{}, err
	}
	g.mu.Lock()
	e, ok := g.execs[exec]
	if !ok {
		g.mu.Unlock()
		return machine.ExecRecord{}, machine.ErrExecAbsent
	}
	if len(e.output) > 0 && after < e.output[0].Offset {
		earliest := e.output[0].Offset
		g.mu.Unlock()
		return machine.ExecRecord{}, &machine.OutputGoneError{Earliest: earliest}
	}
	var chunks []machine.OutputChunk
	for _, c := range e.output {
		if c.Offset+uint64(len(c.Data)) > after {
			chunks = append(chunks, c)
		}
	}
	record := e.record
	g.mu.Unlock()
	for _, c := range chunks {
		if err := send(c); err != nil {
			return machine.ExecRecord{}, err
		}
	}
	return record, nil
}

// PutFile makes the guest of the Machine hold the file.
func (g *Guest) PutFile(id machine.MachineID, path, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files[id] == nil {
		g.files[id] = make(map[string][]byte)
	}
	g.files[id][path] = []byte(content)
}

// PutSpecial makes the path name something that is not a regular file.
func (g *Guest) PutSpecial(id machine.MachineID, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.special[id] == nil {
		g.special[id] = make(map[string]bool)
	}
	g.special[id][path] = true
}

// File returns the content of the file.
func (g *Guest) File(id machine.MachineID, path string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.files[id][path]
	return string(c), ok
}

// read returns the file's content or why it cannot be read.
func (g *Guest) read(id machine.MachineID, path string) ([]byte, error) {
	if g.special[id][path] {
		return nil, machine.ErrNotRegular
	}
	c, ok := g.files[id][path]
	if !ok {
		return nil, machine.ErrFileAbsent
	}
	return c, nil
}

// ReadFile implements machine.Guest, in chunks of four bytes.
func (g *Guest) ReadFile(_ context.Context, id machine.MachineID, path string, send func([]byte) error) error {
	if err := g.enter("read-file"); err != nil {
		return err
	}
	g.mu.Lock()
	content, err := g.read(id, path)
	g.mu.Unlock()
	if err != nil {
		return err
	}
	for rest := content; len(rest) > 0; {
		n := min(4, len(rest))
		if err := send(slices.Clone(rest[:n])); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// WriteFile implements machine.Guest.
func (g *Guest) WriteFile(_ context.Context, id machine.MachineID, path string, content io.Reader) error {
	if err := g.enter("write-file"); err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, content); err != nil {
		return err // the stream broke: the old content stays
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.special[id][path] {
		return machine.ErrNotRegular
	}
	if g.files[id] == nil {
		g.files[id] = make(map[string][]byte)
	}
	g.files[id][path] = buf.Bytes()
	return nil
}

// PatchFile implements machine.Guest. A patch is the new content, which keeps
// the substitute from modelling a patch format.
func (g *Guest) PatchFile(_ context.Context, id machine.MachineID, path, base, expected string, patch []byte) (machine.PatchOutcome, error) {
	if err := g.enter("patch-file"); err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	current, err := g.read(id, path)
	if err != nil {
		return 0, err
	}
	switch Digest(current) {
	case expected:
		return machine.PatchAlreadyApplied, nil
	case base:
		g.files[id][path] = slices.Clone(patch)
		return machine.PatchApplied, nil
	}
	return 0, machine.ErrPatchPrecondition
}

// ListFiles implements machine.Guest: the files directly under path, and the
// directories that hold deeper ones. A path with nothing under it is absent.
func (g *Guest) ListFiles(_ context.Context, id machine.MachineID, path string) ([]machine.FileEntry, error) {
	if err := g.enter("list-files"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	dir := strings.TrimSuffix(path, "/") + "/"
	dirs := make(map[string]bool)
	var out []machine.FileEntry
	for p, content := range g.files[id] {
		rest, under := strings.CutPrefix(p, dir)
		if !under {
			continue
		}
		if name, _, deeper := strings.Cut(rest, "/"); deeper {
			if !dirs[name] {
				dirs[name] = true
				out = append(out, machine.FileEntry{Path: dir + name, Kind: machine.EntryDirectory})
			}
			continue
		}
		kind := machine.EntryFile
		if g.special[id][p] {
			kind = machine.EntryOther
		}
		out = append(out, machine.FileEntry{Path: p, Kind: kind, Size: uint64(len(content))})
	}
	if len(out) == 0 {
		return nil, machine.ErrFileAbsent
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// SearchFiles implements machine.Guest: the files under path whose name
// contains the pattern.
func (g *Guest) SearchFiles(_ context.Context, id machine.MachineID, path, pattern string) ([]string, error) {
	if err := g.enter("search-files"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for p := range g.files[id] {
		if strings.HasPrefix(p, path) && strings.Contains(p[len(path):], pattern) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// EndsMachineWith makes a Terminate end the Machine by calling f, as an agent
// whose entrypoint stops on SIGTERM does. Without it the guest has nothing to
// end a Machine with, and Terminate answers as a call that did not reach an
// agent, so a lifecycle over this substitute falls back to the Driver.
func (g *Guest) EndsMachineWith(f func(machine.MachineID)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ends = f
}

// RefuseTerminate makes Terminate answer err, as an agent that was reached and
// refused the request does: it is not machine.ErrGuestUnreachable. A nil err
// ends the refusal.
func (g *Guest) RefuseTerminate(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = err
}

// Terminate implements machine.Guest.
func (g *Guest) Terminate(ctx context.Context, id machine.MachineID) error {
	if err := g.enter("terminate"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	g.mu.Lock()
	if g.refuse != nil {
		defer g.mu.Unlock()
		return g.refuse
	}
	ends := g.ends
	if ends != nil {
		g.terms[id]++
	}
	g.mu.Unlock()
	if ends == nil {
		return machine.ErrGuestUnreachable
	}
	ends(id)
	return nil
}

// FindDisk implements machine.Guest: the guest finds the disk and records it,
// whether it is new or one it released.
func (g *Guest) FindDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID) error {
	if err := g.enter("find-disk"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.found[id] == nil {
		g.found[id] = make(map[machine.DiskID]bool)
	}
	g.found[id][disk] = true
	delete(g.released[id], disk)
	return nil
}

// ReleaseDisk implements machine.Guest: it releases a disk the guest found,
// unless the disk is held open (HoldDisk). A disk it already released is
// released again without effect, and machine.ErrDiskUnknown is the answer for
// one it never found.
func (g *Guest) ReleaseDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID) error {
	if err := g.enter("release-disk"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released[id][disk] {
		return nil
	}
	if !g.found[id][disk] {
		return machine.ErrDiskUnknown
	}
	if g.held[id][disk] {
		return machine.ErrDiskInUse
	}
	delete(g.found[id], disk)
	if g.released[id] == nil {
		g.released[id] = make(map[machine.DiskID]bool)
	}
	g.released[id][disk] = true
	return nil
}

// PresentVcpus sets how many vCPUs the VMM has presented to the guest of the
// Machine, as a hot-plug that has not finished leaves fewer than the Driver
// asked for. Until it is set, every vCPU asked for is present. A vCPU the VMM
// removes goes offline in the guest, so lowering the count lowers what is online.
func (g *Guest) PresentVcpus(id machine.MachineID, n uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.present[id] = n
	if g.online[id] > n {
		g.online[id] = n
	}
}

// OnlineVcpus implements machine.Guest: the guest onlines the CPUs asked for
// that are present, and reports the count online. It takes none offline: the
// count falls only when the VMM removes a vCPU (PresentVcpus).
func (g *Guest) OnlineVcpus(ctx context.Context, id machine.MachineID, vcpus uint32) (machine.GuestSize, error) {
	if err := g.enter("online-vcpus"); err != nil {
		return machine.GuestSize{}, err
	}
	if err := ctx.Err(); err != nil {
		return machine.GuestSize{}, errors.Join(machine.ErrGuestUnreachable, err)
	}
	if vcpus == 0 {
		return machine.GuestSize{}, errors.New("the guest agent refused 0 vCPUs: at least 1 stays online")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	reach := vcpus
	if n, ok := g.present[id]; ok && n < reach {
		reach = n
	}
	if g.online[id] < reach {
		g.online[id] = reach
	}
	return machine.GuestSize{VCPUs: g.online[id]}, nil
}

// HoldDisk makes the guest use the disk, as a mounted filesystem does, so that
// ReleaseDisk refuses it, or ends the use when open is false.
func (g *Guest) HoldDisk(id machine.MachineID, disk machine.DiskID, open bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held[id] == nil {
		g.held[id] = make(map[machine.DiskID]bool)
	}
	g.held[id][disk] = open
}

// DiskFound reports whether the guest of the Machine has found the disk and not
// released it.
func (g *Guest) DiskFound(id machine.MachineID, disk machine.DiskID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.found[id][disk]
}

// Terminated returns how many Terminate calls the guest of the Machine took.
func (g *Guest) Terminated(id machine.MachineID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.terms[id]
}
