//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// sigkill is the signal number the tests send, as on Linux.
const sigkill = 9

// Signal ends the command it names and no other: the named command's record ends, a command
// signalled later was running until then, and a signal to a command that has ended, to one the
// Machine has no record of, or with a number that is no signal is refused.
func TestE2E_SignalExecEndsTheCommandItNamesAndNoOther(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "signalled")
	// Run without arguments, the program prints a line and stays running.
	for _, id := range []string{"victim", "bystander"} {
		started, err := n.client.Exec(ctx, &v1alpha2.ExecRequest{ExecId: id, RequestId: "exec-" + id, MachineId: "signalled", Command: []string{"/bin/entry"}})
		require.NoError(t, err)
		require.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_STARTED, started.Record.Status, started.Record.Reason)
	}

	_, err := n.client.SignalExec(ctx, &v1alpha2.SignalExecRequest{MachineId: "signalled", ExecId: "victim", Signal: sigkill})
	require.NoError(t, err)

	// A command that was not ended never ends its output, so the read is bounded.
	out, record := n.execEnd(t, ctx, "signalled", "victim")
	assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_EXITED, record.Status, record.Reason)
	assert.Equal(t, int32(sigkill), record.TermSignal, "the record names the signal that ended the command")
	assert.Equal(t, "hello from the entrypoint\n", out, "the command ran before the signal ended it")

	// The signal succeeds only for a command that runs, so the bystander ran until now.
	_, err = n.client.SignalExec(ctx, &v1alpha2.SignalExecRequest{MachineId: "signalled", ExecId: "bystander", Signal: sigkill})
	require.NoError(t, err, "the command that was not named was still running")
	_, record = n.execEnd(t, ctx, "signalled", "bystander")
	assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_EXITED, record.Status, record.Reason)

	for name, req := range map[string]struct {
		exec   string
		signal int32
		kind   v1alpha2.OutcomeKind
	}{
		"a command that has ended":    {"victim", sigkill, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
		"an exec the Machine has not": {"never-run", sigkill, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND},
		"a number that is no signal":  {"bystander", 4242, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED},
	} {
		_, err := n.client.SignalExec(ctx, &v1alpha2.SignalExecRequest{MachineId: "signalled", ExecId: req.exec, Signal: req.signal})
		require.Error(t, err, name)
		assert.Equal(t, req.kind, v1alpha2.KindOf(err), name)
	}
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "signalled"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, inspected.Machine.State, "a signal to a command leaves the Machine as it was")
}

// A command that has exited carries when it ended, by the guest's clock: the start's answer holds
// none, the record that ends the output holds a time between the host's readings taken before the
// Exec and after the read, and the record read again holds the same time.
func TestE2E_AnExitedExecCarriesWhenItEnded(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "timed")

	beforeExec := time.Now()
	started, err := n.client.Exec(ctx, &v1alpha2.ExecRequest{ExecId: "short", RequestId: "exec-short", MachineId: "timed", Command: []string{"/bin/entry", "exit", "3"}})
	require.NoError(t, err)
	assert.Nil(t, started.Record.EndedAt, "a command that has not ended has no end time")
	_, record := n.execEnd(t, ctx, "timed", "short")
	afterRead := time.Now()

	require.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_EXITED, record.Status, record.Reason)
	require.Equal(t, int32(3), record.ExitCode)
	require.NotNil(t, record.EndedAt, "the exited command's record names when it ended")
	endedAt := record.EndedAt.AsTime()
	assert.False(t, endedAt.Before(beforeExec) || endedAt.After(afterRead),
		"the command ended at %v by the guest's clock, outside %v..%v", endedAt, beforeExec, afterRead)

	_, again := n.execEnd(t, ctx, "timed", "short")
	require.NotNil(t, again.EndedAt)
	assert.True(t, endedAt.Equal(again.EndedAt.AsTime()), "a second read gives the time the first did: %v", again.EndedAt.AsTime())
}

// Patch applies at the base digest, a retry at the expected digest succeeds with the content as
// it is, and a base the file is not at is refused with the content as it was.
func TestE2E_PatchFileAppliesAtTheBaseDigestAndRefusesAnotherBase(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "patched", layer(dir("data")))
	n.runMachine(t, ctx, createRequest("patched", pulled.Digest))

	const before, after = "one\ntwo\n", "one\nthree\n"
	const path = "/data/patched.txt"
	n.writeFile(t, ctx, "patched", path, before)
	patch := func(base, expected string) error {
		_, err := n.client.PatchFile(ctx, &v1alpha2.PatchFileRequest{
			MachineId: "patched", Path: path, BaseDigest: sha256Digest([]byte(base)), ExpectedDigest: sha256Digest([]byte(expected)),
			Patch: []byte("--- a\n+++ b\n@@ -1,2 +1,2 @@\n one\n-two\n+three\n"),
		})
		return err
	}

	// The patch and the expected digest are the ones that apply below: only the base is not the
	// file's, so the base is what refuses.
	err := patch(after, after)
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a base the file is not at")
	require.Equal(t, before, n.readFile(t, ctx, "patched", path), "the refused patch changed nothing")

	require.NoError(t, patch(before, after))
	require.Equal(t, after, n.readFile(t, ctx, "patched", path), "the patch applied at the base digest")

	require.NoError(t, patch(before, after), "the file is at the expected digest already")
	assert.Equal(t, after, n.readFile(t, ctx, "patched", path), "the repeat applied nothing")
}

// List and Search stream what the guest holds, sorted by path, over the directory the test made
// and the files it wrote there.
func TestE2E_ListAndSearchFilesStreamTheEntriesTheTestWrote(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "listed-files", layer(dir("data"), dir("data/sub"), dir("data/sub/deep")))
	n.runMachine(t, ctx, createRequest("files", pulled.Digest))
	for path, content := range map[string]string{
		"/data/a.txt":          "alpha",
		"/data/b.log":          "bravo!",
		"/data/sub/c.log":      "charlie",
		"/data/sub/deep/d.log": "delta",
	} {
		n.writeFile(t, ctx, "files", path, content)
	}

	t.Run("ListFiles", func(t *testing.T) {
		entries, err := n.listFiles(ctx, "files", "/data")
		require.NoError(t, err)
		assert.Equal(t, []string{"/data/a.txt FILE 5", "/data/b.log FILE 6", "/data/sub DIRECTORY"}, entries, "the direct entries, by path")

		_, err = n.listFiles(ctx, "files", "/data/a.txt")
		require.Error(t, err)
		assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "a path that is not a directory")
		_, err = n.listFiles(ctx, "files", "/data/absent")
		require.Error(t, err)
		assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, v1alpha2.KindOf(err), "a path that names nothing")
	})

	t.Run("SearchFiles", func(t *testing.T) {
		for pattern, want := range map[string][]string{
			"*.log":       {"/data/b.log"},
			"**/*.log":    {"/data/b.log", "/data/sub/c.log", "/data/sub/deep/d.log"},
			"sub/*.log":   {"/data/sub/c.log"},
			"*.{txt,log}": {"/data/a.txt", "/data/b.log"},
			"[ab].*":      {"/data/a.txt", "/data/b.log"},
		} {
			found, err := n.searchFiles(ctx, "files", "/data", pattern)
			require.NoError(t, err, pattern)
			assert.Equal(t, want, found, "the paths under /data that match %q", pattern)
		}

		_, err := n.searchFiles(ctx, "files", "/data", "[")
		require.Error(t, err)
		assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "a pattern that is not a valid glob")
		_, err = n.searchFiles(ctx, "files", "/data/absent", "*")
		require.Error(t, err)
		assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, v1alpha2.KindOf(err), "a path that names nothing")
	})
}

// A Console session carries what the guest writes to its console after the session opened, and
// none of what it wrote before.
func TestE2E_ConsoleSessionReadsWhatTheGuestWritesAfterItOpens(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "consoled")
	const before, after = "written-before-open", "written-after-open"
	_, code := n.exec(t, ctx, "consoled", "console-before", "/bin/entry", "console", before)
	require.Equal(t, int32(0), code)

	sessionCtx, endSession := context.WithCancel(ctx)
	defer endSession()
	stream, err := n.client.Console(sessionCtx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&v1alpha2.ConsoleRequest{MachineId: "consoled"}))
	type received struct {
		output []byte
		err    error
	}
	outputs := make(chan received)
	go func() {
		for {
			part, err := stream.Recv()
			select {
			case outputs <- received{part.GetOutput(), err}:
			case <-sessionCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Nothing says when the session is connected to the console, and a line written before that
	// is not carried. Each try writes a line of its own and waits for the output to hold it, so a
	// session that connected late reads the next one.
	var seen strings.Builder
	for try := 0; !strings.Contains(seen.String(), after); try++ {
		require.Less(t, try, 20, "no line written after the session opened came back; the output so far: %q", seen.String())
		_, code = n.exec(t, ctx, "consoled", fmt.Sprintf("console-%d", try), "/bin/entry", "console", fmt.Sprintf("%s-%d", after, try))
		require.Equal(t, int32(0), code)
		retry := time.After(5 * time.Second)
	read:
		for !strings.Contains(seen.String(), after) {
			select {
			case r := <-outputs:
				require.NoError(t, r.err, "the output so far: %q", seen.String())
				seen.Write(r.output)
			case <-retry:
				break read
			}
		}
	}
	assert.NotContains(t, seen.String(), before, "what the guest wrote before the session opened is not replayed")
}

// execEnd reads an exec's output until the command has ended, and returns the output and the
// record that ends it.
func (n *node) execEnd(t *testing.T, ctx context.Context, machine, id string) (string, *v1alpha2.ExecRecord) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var out strings.Builder
	record, err := v1alpha2.ReadExecOutput(ctx, n.client, machine, id, 0, func(c *v1alpha2.OutputChunk) error {
		out.Write(c.GetData())
		return nil
	})
	require.NoError(t, err, "the output of %s so far: %q", id, out.String())
	return out.String(), record
}

// listFiles is what List streams for the directory, one string for each entry: its path, its kind
// and, for a file, its size. A refusal arrives with the stream's first read.
func (n *node) listFiles(ctx context.Context, machine, path string) ([]string, error) {
	stream, err := n.client.ListFiles(ctx, &v1alpha2.ListFilesRequest{MachineId: machine, Path: path})
	if err != nil {
		return nil, err
	}
	var entries []string
	for {
		part, err := stream.Recv()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		for _, e := range part.Entries {
			line := fmt.Sprintf("%s %s", e.Path, strings.TrimPrefix(e.Kind.String(), "ENTRY_KIND_"))
			if e.Kind == v1alpha2.EntryKind_ENTRY_KIND_FILE {
				line += fmt.Sprintf(" %d", e.Size)
			}
			entries = append(entries, line)
		}
	}
}

// searchFiles is what Search streams for the directory and the pattern.
func (n *node) searchFiles(ctx context.Context, machine, path, pattern string) ([]string, error) {
	stream, err := n.client.SearchFiles(ctx, &v1alpha2.SearchFilesRequest{MachineId: machine, Path: path, Pattern: pattern})
	if err != nil {
		return nil, err
	}
	var matches []string
	for {
		part, err := stream.Recv()
		if err == io.EOF {
			return matches, nil
		}
		if err != nil {
			return nil, err
		}
		matches = append(matches, part.Matches...)
	}
}
