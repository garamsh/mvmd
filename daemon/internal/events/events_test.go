package events_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

func TestNewStore_EmitsEventsWithGrowingSequence(t *testing.T) {
	store := events.New(100)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_RUNNING)
	store.EmitDeadline(v1alpha1.DeadlineEvent_KIND_ARMED)
	store.EmitDriver("firecracker", "vmm started")

	w := store.Subscribe(0)
	defer w.Close()

	var seqs []uint64
	for e := range w.Events() {
		seqs = append(seqs, e.Sequence)
	}

	assert.Len(t, seqs, 4)
	assert.Equal(t, uint64(1), seqs[0])
	assert.Equal(t, uint64(2), seqs[1])
	assert.Equal(t, uint64(3), seqs[2])
	assert.Equal(t, uint64(4), seqs[3])
}

func TestNewStore_WatcherResumesAfterSequence(t *testing.T) {
	store := events.New(100)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_RUNNING)
	store.EmitStateChanged("m2", v1alpha1.MachineState_MACHINE_STATE_CREATED)

	// Subscribe after sequence 1 — should get seqs 2 and 3.
	w := store.Subscribe(1)
	defer w.Close()

	var ids []string
	for e := range w.Events() {
		ids = append(ids, e.MachineID)
	}

	assert.Equal(t, []string{"m1", "m2"}, ids)
	assert.Equal(t, uint64(3), events.LastSequence(store))
}

func TestNewStore_ResyncRequiredWhenSequenceNotHeld(t *testing.T) {
	store := events.New(100)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)

	// The store only has sequence 1; ask for after 99.
	w := store.Subscribe(99)
	defer w.Close()

	select {
	case <-w.Done():
		// Expected.
	case <-time.After(time.Second):
		t.Fatal("watcher never finished")
	}

	// Events() should return nil for a resync-required watcher.
	assert.Nil(t, w.Events())
}

func TestNewStore_RetentionDropsOldEvents(t *testing.T) {
	store := events.New(3)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_RUNNING)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_PAUSED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_STOPPED)

	// Ask for after 1 — the store only retains 3 events (seqs 2, 3, 4),
	// so the first retained event is after seq 1.
	w := store.Subscribe(1)
	defer w.Close()

	var seqs []uint64
	for e := range w.Events() {
		seqs = append(seqs, e.Sequence)
	}
	assert.Equal(t, []uint64{2, 3, 4}, seqs)
}

func TestNewStore_ResyncWhenRetentionCoversRequestedSequence(t *testing.T) {
	store := events.New(1)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_RUNNING)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_PAUSED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_STOPPED)

	// Retention=1 keeps only seq 4. Ask for after 4 — no events remain,
	// so the client must resync.
	w := store.Subscribe(4)
	defer w.Close()

	select {
	case <-w.Done():
	case <-time.After(time.Second):
		t.Fatal("watcher never finished")
	}
	assert.Nil(t, w.Events())
}

func TestNewStore_MultipleWatchers(t *testing.T) {
	store := events.New(100)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_RUNNING)

	w1 := store.Subscribe(0)
	defer w1.Close()
	w2 := store.Subscribe(0)
	defer w2.Close()

	var c1, c2 int
	for range w1.Events() {
		c1++
	}
	for range w2.Events() {
		c2++
	}

	assert.Equal(t, 2, c1)
	assert.Equal(t, 2, c2)
}

func TestNewStore_StateChangedCarriesMachineID(t *testing.T) {
	store := events.New(100)

	store.EmitStateChanged("unique-id", v1alpha1.MachineState_MACHINE_STATE_FAULTED)

	w := store.Subscribe(0)
	defer w.Close()

	e := <-w.Events()
	assert.Equal(t, "unique-id", e.MachineID)
	_, ok := e.Body.(*v1alpha1.StateChanged)
	assert.True(t, ok)
}

func TestNewStore_DeadlineEventIsEmptyMachineID(t *testing.T) {
	store := events.New(100)

	store.EmitDeadline(v1alpha1.DeadlineEvent_KIND_RENEWED)

	w := store.Subscribe(0)
	defer w.Close()

	e := <-w.Events()
	assert.Empty(t, e.MachineID)
	_, ok := e.Body.(*v1alpha1.DeadlineEvent)
	assert.True(t, ok)
}

func TestNewStore_DriverEventIsEmptyMachineID(t *testing.T) {
	store := events.New(100)

	store.EmitDriver("cloud-hypervisor", "snapshot completed")

	w := store.Subscribe(0)
	defer w.Close()

	e := <-w.Events()
	assert.Empty(t, e.MachineID)
	_, ok := e.Body.(*v1alpha1.DriverEvent)
	assert.True(t, ok)
}

func TestNewStore_WatcherCloseRemovesFromStore(t *testing.T) {
	store := events.New(100)

	w := store.Subscribe(0)
	w.Close()

	// After closing, the watcher should not receive new events.
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)

	select {
	case <-w.Done():
		// The done channel is closed after Close().
	default:
		t.Fatal("Done not closed after Close")
	}

	// The watcher's event channel should be drained.
	select {
	case _, ok := <-w.Events():
		if ok {
			t.Fatal("watcher received an event after being closed")
		}
	default:
	}
}

func TestNewStore_ObserveEmissionUnderConcurrency(t *testing.T) {
	store := events.New(1000)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.EmitStateChanged("m", v1alpha1.MachineState_MACHINE_STATE_CREATED)
			store.EmitDeadline(v1alpha1.DeadlineEvent_KIND_ARMED)
			store.EmitDriver("firecracker", "event")
		}(i)
	}
	wg.Wait()

	w := store.Subscribe(0)
	defer w.Close()

	var count int
	for range w.Events() {
		count++
	}
	assert.Equal(t, 60, count)
}

func TestNewStore_EventsOrderingUnderConcurrency(t *testing.T) {
	store := events.New(1000)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				store.EmitStateChanged("m", v1alpha1.MachineState_MACHINE_STATE_CREATED)
			}
		}()
	}
	wg.Wait()

	w := store.Subscribe(0)
	defer w.Close()

	var prev uint64
	ordered := true
	for e := range w.Events() {
		if prev > 0 && e.Sequence < prev {
			ordered = false
			break
		}
		prev = e.Sequence
	}
	assert.True(t, ordered, "events must be received in non-decreasing sequence order")
}

func TestNewStore_EmptySubscribeReturnsNoEvents(t *testing.T) {
	store := events.New(100)

	w := store.Subscribe(0)
	defer w.Close()

	// No events have been emitted; the watcher should close without
	// any events.
	select {
	case _, ok := <-w.Events():
		assert.False(t, ok, "events channel should be closed immediately")
	case <-time.After(time.Second):
		t.Fatal("events channel never closed")
	}

	select {
	case <-w.Done():
	case <-time.After(time.Second):
		t.Fatal("done never closed")
	}
}

func TestNewStore_SubscribeAfterZeroStartsFromOldest(t *testing.T) {
	store := events.New(100)

	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_CREATED)
	store.EmitStateChanged("m1", v1alpha1.MachineState_MACHINE_STATE_RUNNING)

	w := store.Subscribe(0)
	defer w.Close()

	var seqs []uint64
	for e := range w.Events() {
		seqs = append(seqs, e.Sequence)
	}
	assert.Equal(t, []uint64{1, 2}, seqs)
}
