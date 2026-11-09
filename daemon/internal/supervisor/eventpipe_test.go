package supervisor

import (
	"bytes"
	"io"
	"os"
	"strings"
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
	assert.LessOrEqual(t, r.biggest, pipeReadSize)
	assert.Zero(t, found)

	m := &matcher{match: rebooting}
	for range 100 {
		m.feed(bytes.Repeat([]byte("e"), pipeReadSize))
	}
	assert.Len(t, m.tail, len(rebooting)-1)
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

// An EventPipe is read through a real pipe: the VMM's two writes, the match
// split between them, end it.
func TestEventPipe_AMatchWrittenInTwoPiecesEndsTheVMM(t *testing.T) {
	p, err := OpenEventPipe(rebooting)
	require.NoError(t, err)
	// The VMM's descriptor is a duplicate, since Watch closes the supervisor's
	// own copy of the write end.
	fd, err := unix.Dup(int(p.File().Fd()))
	require.NoError(t, err)
	vmm := os.NewFile(uintptr(fd), "vmm")
	defer vmm.Close()

	ended := make(chan struct{})
	var log bytes.Buffer
	p.Watch(func() { close(ended) }, &log)

	half := len(rebooting) / 2
	_, err = vmm.Write(append([]byte(`{"source": "vm", `), rebooting[:half]...))
	require.NoError(t, err)
	select {
	case <-ended:
		t.Fatal("ended before the match was whole")
	case <-time.After(100 * time.Millisecond):
	}
	_, err = vmm.Write(append(append([]byte(nil), rebooting[half:]...), "}\n\n"...))
	require.NoError(t, err)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the VMM was not ended")
	}
}

// A description with no pipe makes none and passes the descriptors the child
// has always had; one with a pipe puts its write end at the descriptor it names.
func TestExtraFiles_AnEmptyFieldIsNoPipe(t *testing.T) {
	files, events, err := extraFiles(&pb.Process{}, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, events)
	assert.Len(t, files, 4)

	files, events, err = extraFiles(&pb.Process{EndOn: &pb.EndOn{Fd: 9, Match: rebooting}}, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, events)
	defer events.Close()
	require.Len(t, files, 7)
	assert.Same(t, events.File(), files[6], "descriptor 9 is index 6 of the extra files")
	assert.Nil(t, files[4])
	assert.Nil(t, files[5])
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
