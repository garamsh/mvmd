package convertchannel_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/convertchannel"
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// These tests drive the adapter against the real converter, the host-side
// binary converter-channel, over a Unix socket, with the real mkfs.erofs and
// the real fsck.erofs that reads what it wrote. `make test` builds the binary
// and names all three in CONVERTER_CHANNEL, MKFS_EROFS and FSCK_EROFS; a run
// without them fails, since a pass that never reached the converter would say
// nothing about the adapter.

const uuid = "11111111-2222-3333-4444-555555555555"

// socketConnector is the dialer a Firecracker Driver has for the host side of
// a guest's vsock: it connects to the Unix socket, writes `CONNECT <port>`,
// and reads the `OK <port>` line before any byte of the gRPC stream.
type socketConnector struct{ path string }

func (c socketConnector) GuestConn(ctx context.Context, _ machine.MachineID, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
	}
	// One byte at a time: nothing of the stream behind the line is consumed.
	var line []byte
	one := make([]byte, 1)
	for len(line) == 0 || line[len(line)-1] != '\n' {
		if _, err := conn.Read(one); err != nil {
			_ = conn.Close()
			return nil, errors.Join(machine.ErrGuestUnreachable, err)
		}
		line = append(line, one[0])
	}
	if want := fmt.Sprintf("OK %d\n", port); string(line) != want {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: the host side answered %q", machine.ErrGuestUnreachable, line)
	}
	return conn, nil
}

// plainConnector opens the socket and sends no handshake.
type plainConnector struct{ path string }

func (c plainConnector) GuestConn(ctx context.Context, _ machine.MachineID, _ uint32) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.path)
}

func tool(t *testing.T, variable string) string {
	t.Helper()
	path := os.Getenv(variable)
	if path == "" {
		t.Fatalf("%s names no binary: run these tests through `make test`, which builds it and sets it", variable)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is absent (%v): run these tests through `make test`, which builds it", path, err)
	}
	return path
}

// realConverter is the binary started on a socket in a temporary directory,
// with an output file of 64 MiB, and the socket and output it serves.
type realConverter struct {
	socket string
	output string
	dir    string
}

func startConverter(t *testing.T) realConverter {
	t.Helper()
	bin := tool(t, "CONVERTER_CHANNEL")
	mkfs := tool(t, "MKFS_EROFS")
	dir, err := os.MkdirTemp("", "conv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rc := realConverter{socket: filepath.Join(dir, "c.sock"), output: filepath.Join(dir, "output.img"), dir: dir}
	f, err := os.Create(rc.output)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(64<<20))
	require.NoError(t, f.Close())

	cmd := exec.Command(bin, "--mkfs", mkfs, "--output", rc.output, "--handshake-port", fmt.Sprint(image.ConverterPort), rc.socket)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(rc.socket); return err == nil }, 10*time.Second, 10*time.Millisecond, "the binary never bound its socket")
	return rc
}

// tarLayer is a tar archive of the files, which mkfs.erofs and the converter's
// reader both take.
func tarLayer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, content := range files {
		require.NoError(t, w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(content)), Format: tar.FormatUSTAR}))
		_, err := w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestRealConverter_ConvertsALayerAndReportsTheLengthOfTheEROFS(t *testing.T) {
	rc := startConverter(t)
	layer := layerFile(t, tarType, tarLayer(t, map[string]string{"a": "A", "b": "BB"}))
	c := convertchannel.New(socketConnector{rc.socket})

	length, err := c.Convert(context.Background(), "convert-x", jobOf(layer))
	require.NoError(t, err)
	require.NotZero(t, length)

	// The output holds the EROFS, as long as the converter said, and fsck.erofs
	// reads back the files the layer held.
	require.NoError(t, os.Truncate(rc.output, int64(length)))
	extracted := filepath.Join(rc.dir, "extracted")
	require.NoError(t, os.Mkdir(extracted, 0o700))
	out, err := exec.Command(tool(t, "FSCK_EROFS"), "--extract="+extracted, rc.output).CombinedOutput()
	require.NoError(t, err, string(out))
	for name, want := range map[string]string{"a": "A", "b": "BB"} {
		got, err := os.ReadFile(filepath.Join(extracted, name))
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}
}

// The real converter refuses a layer type it does not take as a failure with
// its step. The control is the same job with a type it takes, which converts.
func TestRealConverter_AFailureIsAStepError(t *testing.T) {
	data := tarLayer(t, map[string]string{"a": "A"})
	job := func(mediaType string) image.ConversionJob {
		return jobOf(layerFile(t, mediaType, data))
	}

	rc := startConverter(t)
	_, err := convertchannel.New(socketConnector{rc.socket}).Convert(context.Background(), "m", job("application/vnd.oci.image.layer.v1.tar+brotli"))
	var failure *image.ConversionFailure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, image.StepLayerType, failure.Step)

	rc = startConverter(t)
	_, err = convertchannel.New(socketConnector{rc.socket}).Convert(context.Background(), "m", job(tarType))
	require.NoError(t, err)
}

// The converter serves one conversion and stops: a second conversion on the
// same VM finds no converter, which is an unreachable VM.
func TestRealConverter_ASecondConversionFindsNoConverter(t *testing.T) {
	rc := startConverter(t)
	c := convertchannel.New(socketConnector{rc.socket})
	job := jobOf(layerFile(t, tarType, tarLayer(t, map[string]string{"a": "A"})))

	_, err := c.Convert(context.Background(), "m", job)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		// The converter no longer listens, so each attempt waits out its own
		// bound.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = c.Convert(ctx, "m", job)
		return errors.Is(err, machine.ErrGuestUnreachable)
	}, 15*time.Second, 100*time.Millisecond, "the converter kept serving: %v", err)
}

// A connection that opens without the handshake gets no converter: the binary
// closes it, which is an unreachable VM, as it is for the Guest Agent.
func TestRealConverter_NoHandshakeIsUnreachable(t *testing.T) {
	rc := startConverter(t)
	job := jobOf(layerFile(t, tarType, tarLayer(t, map[string]string{"a": "A"})))

	// The connection is closed each time, so the call waits for a converter
	// that never answers until its bound.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := convertchannel.New(plainConnector{rc.socket}).Convert(ctx, "m", job)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)

	// Control: the same converter, with the handshake, converts.
	_, err = convertchannel.New(socketConnector{rc.socket}).Convert(context.Background(), "m", job)
	require.NoError(t, err)
}
