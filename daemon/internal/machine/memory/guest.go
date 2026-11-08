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
// "boot", "update-network", "exec", "read-exec-output", "read-file",
// "write-file", "patch-file" and "search-files"; a failing step answers
// machine.ErrGuestUnreachable unless told another error.
type Guest struct {
	Failures

	mu        sync.Mutex
	files     map[machine.MachineID]map[string][]byte
	special   map[machine.MachineID]map[string]bool
	execs     map[machine.ExecID]*guestExec
	network   map[machine.MachineID]machine.GuestNetwork
	boots     map[machine.MachineID]machine.GuestBoot
	retention int
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
		network:   make(map[machine.MachineID]machine.GuestNetwork),
		boots:     make(map[machine.MachineID]machine.GuestBoot),
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

// Boot implements machine.Guest.
func (g *Guest) Boot(_ context.Context, id machine.MachineID, boot machine.GuestBoot) error {
	if err := g.enter("boot"); err != nil {
		return err
	}
	g.mu.Lock()
	g.boots[id] = boot
	g.mu.Unlock()
	return nil
}

// UpdateNetwork implements machine.Guest.
func (g *Guest) UpdateNetwork(_ context.Context, id machine.MachineID, network machine.GuestNetwork) error {
	if err := g.enter("update-network"); err != nil {
		return err
	}
	g.mu.Lock()
	g.network[id] = network
	g.mu.Unlock()
	return nil
}

// Network returns what the guest of the Machine was last told of its network.
func (g *Guest) Network(id machine.MachineID) (machine.GuestNetwork, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.network[id]
	return n, ok
}

// Exec implements machine.Guest: the command starts and runs until Finish.
func (g *Guest) Exec(_ context.Context, _ machine.MachineID, exec machine.ExecID, _ machine.ExecSpec) (machine.ExecRecord, error) {
	if err := g.enter("exec"); err != nil {
		return machine.ExecRecord{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.execs[exec]; ok {
		return e.record, nil
	}
	g.execs[exec] = &guestExec{record: machine.ExecRecord{ID: exec, Status: machine.ExecStarted}}
	return g.execs[exec].record, nil
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
