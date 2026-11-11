package app_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// watch opens WatchEvents after the sequence on the wired daemon.
func watch(t *testing.T, n *node, after uint64) v1alpha2.RuntimeService_WatchEventsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	s, err := n.client.WatchEvents(ctx, &v1alpha2.WatchEventsRequest{AfterSequence: after})
	require.NoError(t, err)
	return s
}

func recv(t *testing.T, s v1alpha2.RuntimeService_WatchEventsClient) *v1alpha2.WatchEventsResponse {
	t.Helper()
	e, err := s.Recv()
	require.NoError(t, err)
	return e
}

func recvN(t *testing.T, s v1alpha2.RuntimeService_WatchEventsClient, n int) []*v1alpha2.WatchEventsResponse {
	t.Helper()
	out := make([]*v1alpha2.WatchEventsResponse, n)
	for i := range out {
		out[i] = recv(t, s)
	}
	return out
}

// requireWaiting requires the stream to deliver nothing for a moment: the
// server holds it open and does not end it or send a resync.
func requireWaiting(t *testing.T, s v1alpha2.RuntimeService_WatchEventsClient) (got chan *v1alpha2.WatchEventsResponse) {
	t.Helper()
	got = make(chan *v1alpha2.WatchEventsResponse, 1)
	go func() {
		e, err := s.Recv()
		if err != nil {
			got <- nil
			return
		}
		got <- e
	}()
	select {
	case e := <-got:
		t.Fatalf("the stream delivered %v while no event was emitted", e)
	case <-time.After(100 * time.Millisecond):
	}
	return got
}

func sequences(events []*v1alpha2.WatchEventsResponse) []uint64 {
	out := make([]uint64, len(events))
	for i, e := range events {
		out[i] = e.GetSequence()
	}
	return out
}

func raise(n *node, count int, from int) {
	for i := from; i < from+count; i++ {
		n.app.Substitutes.Driver.Raise("tick", fmt.Sprint(i))
	}
}

// What each call of a Machine's life puts on the stream, as it happens and as
// a later watcher replays it, with the states as the wire names them.
func TestEvents_ALiveStreamCarriesEveryChangeAndAReplayIsTheSameSequence(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)

	live := watch(t, n, 0)
	got := requireWaiting(t, live)

	_, err := n.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err)
	renew(t, n, 1)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	_, err = n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	_, err = n.client.ResumeMachine(ctx, &v1alpha2.ResumeMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(time.Second)})
	require.NoError(t, err)

	events := append([]*v1alpha2.WatchEventsResponse{<-got}, recvN(t, live, 5)...)
	assert.Equal(t, []uint64{1, 2, 3, 4, 5, 6}, sequences(events))

	// The second event is the deadline's arming; the rest are the states.
	assert.Empty(t, events[1].GetMachineId(), "a deadline event belongs to the node")
	assert.Equal(t, v1alpha2.DeadlineEvent_KIND_ARMED, events[1].GetDeadline().GetKind())
	states := map[int]v1alpha2.MachineState{
		0: v1alpha2.MachineState_MACHINE_STATE_CREATED,
		2: v1alpha2.MachineState_MACHINE_STATE_RUNNING,
		3: v1alpha2.MachineState_MACHINE_STATE_PAUSED,
		4: v1alpha2.MachineState_MACHINE_STATE_RUNNING,
		5: v1alpha2.MachineState_MACHINE_STATE_STOPPED,
	}
	for i, want := range states {
		assert.Equal(t, "m", events[i].GetMachineId())
		assert.Equal(t, want, events[i].GetStateChanged().GetState(), "event %d", events[i].GetSequence())
	}

	// Control: a watcher that arrives later gets the same events in the same
	// order, so a live stream and a replay are one sequence.
	replay := recvN(t, watch(t, n, 0), 6)
	for i := range events {
		assert.Equal(t, events[i].String(), replay[i].String())
	}
}

// A deleted Machine is announced on the stream, after the states it went
// through, and a Machine that is not deleted is not.
func TestEvents_ADeletedMachineIsOnTheStream(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)
	s := watch(t, n, 0)

	_, err := n.client.CreateMachine(ctx, create("gone"))
	require.NoError(t, err)
	_, err = n.client.CreateMachine(ctx, create("control"))
	require.NoError(t, err)
	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "gone"})
	require.NoError(t, err)

	events := recvN(t, s, 3)
	assert.Equal(t, "gone", events[0].GetMachineId())
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_CREATED, events[0].GetStateChanged().GetState())
	assert.Equal(t, "control", events[1].GetMachineId())
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_CREATED, events[1].GetStateChanged().GetState())
	assert.Equal(t, "gone", events[2].GetMachineId())
	assert.NotNil(t, events[2].GetMachineDeleted(), "the deletion is its own event")
	assert.Nil(t, events[2].GetStateChanged(), "a deleted Machine has no state")

	// Control: the Machine that was not deleted receives no MachineDeleted.
	for _, e := range events {
		if e.GetMachineId() == "control" {
			assert.Nil(t, e.GetMachineDeleted())
		}
	}
	requireWaiting(t, s) // and nothing more followed
}

func TestEvents_AWatcherResumesAfterASequence(t *testing.T) {
	n := start(t, testConfig(t))
	raise(n, 5, 1)

	resumed := recvN(t, watch(t, n, 3), 2)
	assert.Equal(t, []uint64{4, 5}, sequences(resumed))
	assert.Equal(t, "4", resumed[0].GetDriver().GetDetail())

	// Control: zero starts at the oldest held.
	assert.Equal(t, []uint64{1, 2}, sequences(recvN(t, watch(t, n, 0), 2)))
}

func TestEvents_AWatcherAtTheHeadWaitsForTheNextEvent(t *testing.T) {
	n := start(t, testConfig(t))
	raise(n, 3, 1)

	s := watch(t, n, 3)
	got := requireWaiting(t, s)
	raise(n, 1, 4)
	e := <-got
	require.NotNil(t, e, "the stream ended or failed instead of waiting")
	assert.Equal(t, uint64(4), e.GetSequence())
	assert.Equal(t, "4", e.GetDriver().GetDetail())
}

func TestEvents_ResyncRequiredWhenTheSequenceIsNoLongerHeld(t *testing.T) {
	n := start(t, testConfig(t))
	raise(n, 1100, 1) // more than the node holds

	// The oldest event held, whatever the node's retention is.
	oldest := recv(t, watch(t, n, 0)).GetSequence()
	require.Greater(t, oldest, uint64(2), "the setup: events were dropped")

	gone := watch(t, n, oldest-2)
	resync := recv(t, gone)
	assert.NotNil(t, resync.GetResyncRequired())
	assert.Zero(t, resync.GetSequence())
	_, err := gone.Recv()
	assert.Equal(t, io.EOF, err, "the stream ends after the resync")

	// Control: one sequence later nothing was dropped after it, so the stream
	// resumes with the oldest event held.
	held := recv(t, watch(t, n, oldest-1))
	assert.Nil(t, held.GetResyncRequired())
	assert.Equal(t, oldest, held.GetSequence())

	// And a sequence this node never reached is not held either.
	ahead := recv(t, watch(t, n, 5000))
	assert.NotNil(t, ahead.GetResyncRequired())
}

func TestEvents_AWatcherThatFallsBehindEndsWithResyncAndNoHole(t *testing.T) {
	n := start(t, testConfig(t))
	s := watch(t, n, 0)
	raise(n, 1, 1)
	assert.Equal(t, uint64(1), recv(t, s).GetSequence())

	// The client reads nothing while far more events than the transport can
	// buffer are emitted, so the server's sends block and the watcher falls
	// behind the retention. What the client then reads must not skip one.
	const emitted = 500_000
	raise(n, emitted, 2)
	last, resynced := uint64(1), false
	for !resynced && last < emitted+1 {
		e, err := s.Recv()
		require.NoError(t, err)
		if resynced = e.GetResyncRequired() != nil; !resynced {
			assert.Equal(t, last+1, e.GetSequence(), "a gap in the stream")
			last = e.GetSequence()
		}
	}
	assert.True(t, resynced, "the setup: the watcher fell behind, and was told so")
	_, err := s.Recv()
	assert.Equal(t, io.EOF, err)
}

func TestEvents_TheDriversEventsAreOnTheStream(t *testing.T) {
	n := start(t, testConfig(t))
	s := watch(t, n, 0)
	n.app.Substitutes.Driver.Raise("vmm-exited", "signal 9")

	e := recv(t, s)
	assert.Equal(t, "vmm-exited", e.GetDriver().GetName())
	assert.Equal(t, "signal 9", e.GetDriver().GetDetail())
	assert.Empty(t, e.GetMachineId())
}

func TestEvents_TheExpiryIsOnTheStream(t *testing.T) {
	n := start(t, testConfig(t))
	s := watch(t, n, 0)
	_, err := n.client.RenewDeadline(context.Background(), &v1alpha2.RenewDeadlineRequest{
		Length: durationpb.New(50 * time.Millisecond), RenewalNumber: 1,
	})
	require.NoError(t, err)

	events := recvN(t, s, 2)
	assert.Equal(t, v1alpha2.DeadlineEvent_KIND_ARMED, events[0].GetDeadline().GetKind())
	assert.Equal(t, v1alpha2.DeadlineEvent_KIND_EXPIRED, events[1].GetDeadline().GetKind())
}

func TestEvents_ARenewalIsOnTheStreamAsRenewed(t *testing.T) {
	n := start(t, testConfig(t))
	s := watch(t, n, 0)
	for number := uint64(1); number <= 2; number++ {
		_, err := n.client.RenewDeadline(context.Background(), &v1alpha2.RenewDeadlineRequest{
			Length: durationpb.New(time.Hour), RenewalNumber: number,
		})
		require.NoError(t, err)
	}

	events := recvN(t, s, 2)
	assert.Equal(t, v1alpha2.DeadlineEvent_KIND_ARMED, events[0].GetDeadline().GetKind())
	assert.Equal(t, v1alpha2.DeadlineEvent_KIND_RENEWED, events[1].GetDeadline().GetKind())
}

// Machines changing state together are one consecutive sequence, and each
// Machine's own events are in the order its states were committed.
func TestEvents_ConcurrentStateChangesAreOneOrderedSequence(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)
	renew(t, n, 1)
	s := watch(t, n, 0)

	const machines, perMachine = 8, 5
	var wg sync.WaitGroup
	for i := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint("m", i)
			for _, call := range []func() error{
				func() error { _, err := n.client.CreateMachine(ctx, create(id)); return err },
				func() error {
					_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id})
					return err
				},
				func() error {
					_, err := n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: id})
					return err
				},
				func() error {
					_, err := n.client.ResumeMachine(ctx, &v1alpha2.ResumeMachineRequest{MachineId: id})
					return err
				},
				func() error {
					_, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: id, GracePeriod: durationpb.New(time.Second)})
					return err
				},
			} {
				assert.NoError(t, call())
			}
		}()
	}
	wg.Wait()

	events := recvN(t, s, 1+machines*perMachine) // the arming, then the changes
	for i, e := range events {
		assert.Equal(t, uint64(i+1), e.GetSequence(), "one consecutive sequence")
	}
	byMachine := map[string][]v1alpha2.MachineState{}
	for _, e := range events[1:] {
		byMachine[e.GetMachineId()] = append(byMachine[e.GetMachineId()], e.GetStateChanged().GetState())
	}
	require.Len(t, byMachine, machines)
	for id, got := range byMachine {
		assert.Equal(t, []v1alpha2.MachineState{
			v1alpha2.MachineState_MACHINE_STATE_CREATED,
			v1alpha2.MachineState_MACHINE_STATE_RUNNING,
			v1alpha2.MachineState_MACHINE_STATE_PAUSED,
			v1alpha2.MachineState_MACHINE_STATE_RUNNING,
			v1alpha2.MachineState_MACHINE_STATE_STOPPED,
		}, got, "events of %s", id)
	}
}

// A stream has no end of its own: stopping the daemon ends it, and the stop
// does not wait for the client.
func TestEvents_StoppingTheDaemonEndsOpenStreams(t *testing.T) {
	n := start(t, testConfig(t))
	s := watch(t, n, 0)
	n.app.Substitutes.Driver.Raise("tick", "1")
	recv(t, s) // the server is inside the stream

	// The client keeps its connection, so only the server can end the stream.
	t.Cleanup(func() { _ = n.conn.Close() })
	n.stop()
	select {
	case err := <-n.done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop with a stream open")
	}
	n.stop = nil
	_, err := s.Recv()
	assert.Equal(t, io.EOF, err, "the stream ended cleanly, so the client may resume")
}
