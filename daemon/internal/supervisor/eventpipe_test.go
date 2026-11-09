package supervisor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

var rebooting = []byte(`"event": "rebooting"`)

// A match that spans two reads is found: the stream arrives a byte at a time,
// so no read holds the whole of it. The control is the same stream with one
// byte of the match altered, which is not found.
func TestWatch_AMatchSpanningReadsIsFound(t *testing.T) {
	stream := `{"source": "vm", "event": "rebooting"}` + "\n\n"
	var log bytes.Buffer
	found := 0
	watch(iotest.OneByteReader(strings.NewReader(stream)), rebooting, func() { found++ }, &log)
	assert.Equal(t, 1, found)
	assert.Contains(t, log.String(), "ending the VMM")

	log.Reset()
	found = 0
	watch(iotest.OneByteReader(strings.NewReader(strings.Replace(stream, "rebooting", "rebootinx", 1))), rebooting, func() { found++ }, &log)
	assert.Zero(t, found, "a stream that does not hold the match ends nothing")
	assert.Empty(t, log.String())
}

// Pieces the pipe is read in are bounded and what is carried between them is
// less than the match, however much the stream holds.
type countingReader struct {
	left, biggest int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	c.biggest = max(c.biggest, len(p))
	n := min(len(p), c.left)
	copy(p, bytes.Repeat([]byte("e"), n))
	c.left -= n
	return n, nil
}

func TestWatch_TheReadIsBoundedAndTheStreamIsReadToItsEnd(t *testing.T) {
	r := &countingReader{left: 8 << 20}
	found := 0
	watch(r, rebooting, func() { found++ }, io.Discard)
	assert.Zero(t, r.left, "the stream is read to its end, so the VMM never blocks on the pipe")
	assert.LessOrEqual(t, r.biggest, 4096)
	assert.Zero(t, found)

	m := &matcher{match: rebooting}
	for range 100 {
		m.feed(bytes.Repeat([]byte("e"), 4096))
	}
	assert.Len(t, m.buf, len(rebooting)-1)
}

// Reading goes on after a match, and the end runs once however many times the
// match is written.
func TestWatch_TheEndRunsOnceAndTheReadContinues(t *testing.T) {
	r := strings.NewReader(strings.Repeat(string(rebooting)+"\n", 3) + strings.Repeat("x", 10000))
	found := 0
	watch(r, rebooting, func() { found++ }, io.Discard)
	assert.Equal(t, 1, found)
	assert.Zero(t, r.Len())
}

// The pipe ends with the process: once the last write end is closed the reader
// ends and closes the read end. The control is the pipe while the process still
// holds a write end, which is still being read.
func TestEventPipe_TheReaderEndsWhenTheProcessClosesItsWriteEnd(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	fd, err := unix.Dup(int(w.Fd()))
	require.NoError(t, err)
	vmm := os.NewFile(uintptr(fd), "vmm")
	defer vmm.Close()

	e := &eventPipe{r: r, w: w, match: rebooting}
	e.watch(func() {}, io.Discard)

	_, err = r.Stat()
	require.NoError(t, err, "control: while the process holds its end the pipe is read")
	require.NoError(t, vmm.Close())
	require.Eventually(t, func() bool {
		_, err := r.Stat()
		return errors.Is(err, os.ErrClosed)
	}, 5*time.Second, 10*time.Millisecond, "the reader outlived the process")
}

// endOn runs script as a process with EndOnPipe's pipe at descriptor fd, as the
// supervisor and the drivertest harness start one, and returns how it ended
// and what was logged.
func endOn(t *testing.T, fd int, script string) (*os.ProcessState, string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.ExtraFiles = make([]*os.File, 4)
	var log bytes.Buffer
	started, err := EndOnPipe(cmd, fd, rebooting, &log)
	require.NoError(t, err)
	started(cmd.Start())
	_ = cmd.Wait()
	return cmd.ProcessState, log.String()
}

// A process that writes the match to its descriptor, in two pieces, is killed
// and the log says why; one that writes something else is not. Descriptor 9 is
// past the files the command already passes.
func TestEndOnPipe_AProcessThatWritesTheMatchIsKilledAndOneThatDoesNotIsNot(t *testing.T) {
	state, log := endOn(t, 9, `printf '{"source": "vm", "event": "reb' >&9; sleep 0.2; printf 'ooting"}\n\n' >&9; sleep 30`)
	assert.False(t, state.Success())
	assert.Equal(t, syscall.SIGKILL, state.Sys().(syscall.WaitStatus).Signal())
	assert.Contains(t, log, "ending the VMM")

	state, log = endOn(t, 9, `printf '{"source": "vm", "event": "booted"}\n\n' >&9; sleep 1`)
	assert.True(t, state.Success(), "control: an event that is not the match ends nothing")
	assert.Empty(t, log)
}

// An empty field is no pipe: the files the command passes are as they were.
func TestPlaceEndOn_AnEmptyFieldIsNoPipe(t *testing.T) {
	cmd := exec.Command("true")
	cmd.ExtraFiles = make([]*os.File, 4)
	started, err := placeEndOn(cmd, nil, io.Discard)
	require.NoError(t, err)
	started(nil)
	assert.Len(t, cmd.ExtraFiles, 4)

	started, err = placeEndOn(cmd, &pb.EndOn{Fd: 7, Match: rebooting}, io.Discard)
	require.NoError(t, err)
	started(errors.New("not started"))
	assert.Len(t, cmd.ExtraFiles, 5, "control: a field with a pipe places its write end")
}

func TestEndOnPipe_RefusesWhatItCannotPlace(t *testing.T) {
	cmd := exec.Command("true")
	cmd.ExtraFiles = make([]*os.File, 5)
	_, err := EndOnPipe(cmd, 7, rebooting, io.Discard)
	assert.ErrorContains(t, err, "already passes")
	_, err = EndOnPipe(exec.Command("true"), 7, nil, io.Discard)
	assert.ErrorContains(t, err, "no bytes to match")
	_, err = EndOnPipe(exec.Command("true"), -1, rebooting, io.Discard)
	assert.Error(t, err)
}

func TestCheckEndOn_RefusesAPipeTheSupervisorCannotGive(t *testing.T) {
	assert.NoError(t, checkEndOn(7, rebooting))
	assert.Error(t, checkEndOn(6, rebooting), "6 is the console's")
	assert.Error(t, checkEndOn(0, rebooting))
	assert.Error(t, checkEndOn(maxEndOnFD+1, rebooting))
	assert.Error(t, checkEndOn(7, nil), "a descriptor with nothing to match")
	assert.Error(t, checkEndOn(7, bytes.Repeat([]byte("a"), maxEndOnMatch+1)))

	req := validRequest()
	req.Process.EndOn = &pb.EndOn{Fd: 7, Match: rebooting}
	require.NoError(t, checkRequest(req), "control: a request with a pipe is accepted")
	req.Process.EndOn.Match = nil
	assert.ErrorContains(t, checkRequest(req), "no bytes to match")
}
