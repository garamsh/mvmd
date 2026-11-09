package image_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// While the converter converts, its VM is a VMM process under the supervisor,
// run as a user of its own, booted from one read-write output disk and no
// network device, in the run directory the supervisor names.
func TestPull_ConverterVMIsBootedConfinedAndEndedThroughThePorts(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	id := conversionID(ref)
	var during struct {
		running bool
		user    machine.User
		boot    machine.BootSpec
		process machine.ProcessDescription
		held    int
		layers  [][]byte
	}
	r.conv.Serve(func(_ context.Context, got machine.MachineID, job image.ConversionJob) (uint64, error) {
		assert.Equal(t, id, got)
		during.process, during.running = r.sup.Running(id)
		during.user, _ = r.sup.User(id)
		during.boot = r.driver.BootedWith(id)
		during.held = r.users.Held()
		for _, l := range job.Layers {
			content, _ := r.store.Layer(l.Path)
			during.layers = append(during.layers, content)
		}
		return memory.DefaultLength, nil
	})

	require.NoError(t, r.pull(ref))

	require.True(t, during.running, "the VMM was not under the supervisor while the converter ran")
	assert.Equal(t, machine.User{UID: memory.FirstUID, GID: memory.FirstUID}, during.user)
	assert.Equal(t, 1, during.held)
	require.Len(t, during.boot.Devices, 1, "the converter's only device is its output disk")
	assert.False(t, during.boot.Devices[0].ReadOnly)
	assert.Equal(t, "/staging/staging-1/output", during.boot.Devices[0].Path)
	assert.Equal(t, machine.NetworkDevice{}, during.boot.Network, "the converter has no network device")
	assert.Empty(t, during.process.Tap)
	assert.Equal(t, r.settings.Resources, during.boot.Resources)
	runDir, err := r.sup.RunDir(id)
	require.NoError(t, err)
	assert.Equal(t, runDir, during.boot.RunDir)
	assert.Equal(t, runDir, during.process.RunDir, "the supervisor is given the run directory the Driver was")
	assert.Equal(t, [][]byte{[]byte("base layer of one"), []byte("top layer of one")}, during.layers)

	_, running := r.sup.Running(id)
	assert.False(t, running, "the VMM outlived the conversion")
	assert.Zero(t, r.users.Held())
}

// Two conversions at once are two VMs, each with a user of its own.
func TestPull_ConcurrentConversionsOfDifferentImagesHaveDifferentUsers(t *testing.T) {
	r := newRig(t)
	a, b := r.add("a"), r.add("b")
	entered, release := r.conv.Hold("convert")
	var userA, userB machine.User
	bConverted := make(chan struct{})
	r.conv.Serve(func(_ context.Context, id machine.MachineID, _ image.ConversionJob) (uint64, error) {
		switch id {
		case conversionID(a):
			userA, _ = r.sup.User(id)
		case conversionID(b):
			userB, _ = r.sup.User(id)
			close(bConverted)
		}
		return memory.DefaultLength, nil
	})

	errs := make(chan error, 2)
	go func() { errs <- r.pull(a) }()
	<-entered // a's conversion is waiting, its VMM running and its user held
	go func() { errs <- r.pull(b) }()
	select {
	case <-bConverted:
	case <-time.After(5 * time.Second):
		t.Fatal("the second conversion did not run beside the first")
	}
	release()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)

	assert.NotEqual(t, userA, userB)
	assert.NotZero(t, userA.UID)
	assert.NotZero(t, userB.UID)
	assert.Zero(t, r.users.Held())
}

// A conversion that outlasts the node's limit fails the pull and the VMM is
// ended: the limit is a bound of the call, as well as the supervisor's expiry.
func TestPull_ConversionPastTheLimitFails(t *testing.T) {
	waitForTheLimit := func(ctx context.Context, _ machine.MachineID, _ image.ConversionJob) (uint64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	r := newRig(t)
	r.settings.Limit = 20 * time.Millisecond
	r.start()
	ref := r.add("one")
	r.conv.Serve(waitForTheLimit)

	err := requireKind(t, r.pull(ref), machine.ErrFailed)

	assert.Contains(t, err.Reason, context.DeadlineExceeded.Error())
	assert.Zero(t, r.calls("seal"))
	r.requireNothingVisible(t, ref)

	// The control: a conversion that finishes inside the limit succeeds.
	r.conv.Serve(nil)
	require.NoError(t, r.pull(ref))
}

// A pull that outlasts its own bound ends with the VMM ended and the staging
// area removed, though the context it ran on is the one that expired.
func TestPull_PullBoundEndsAHangingPullAndCleansUp(t *testing.T) {
	r := newRig(t, image.WithPullTimeout(20*time.Millisecond))
	ref := r.add("one")
	r.conv.Serve(func(ctx context.Context, _ machine.MachineID, _ image.ConversionJob) (uint64, error) {
		<-ctx.Done()
		return 0, errors.New("converter cut off: " + ctx.Err().Error())
	})

	err := r.pull(ref)

	requireKind(t, err, machine.ErrFailed)
	r.requireNothingVisible(t, ref)

	// The control: the same bound lets a prompt conversion finish.
	r.conv.Serve(nil)
	require.NoError(t, r.pull(ref))
}
