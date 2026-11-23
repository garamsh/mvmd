package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
)

// refusing is the Machines' supervisors, of which one is not spoken to.
type refusing struct {
	fencing.Supervisors
	err error
}

func (r refusing) CheckCompatible(context.Context) error { return r.err }

// A supervisor this mvmd does not speak to is found before anything is served or changed: Serve
// returns the refusal, opens no socket, and the control is the same node over supervisors it
// speaks to (ADR 0070).
func TestServe_RefusesBeforeServingWhenASupervisorIsNotSpokenTo(t *testing.T) {
	cfg := config(t)
	a, err := New(cfg)
	require.NoError(t, err)
	refusal := errors.New("the supervisor of Machine \"m\" speaks supervisor API \"v2\"")
	a.supervisors = refusing{Supervisors: a.supervisors, err: refusal}

	err = a.Serve(context.Background())

	require.ErrorIs(t, err, refusal)
	assert.NoFileExists(t, cfg.Socket.Path, "nothing was served")
	select {
	case <-a.Ready():
		t.Fatal("the daemon became ready")
	default:
	}

	cfg = config(t)
	a, err = New(cfg)
	require.NoError(t, err)
	a.supervisors = refusing{Supervisors: a.supervisors}
	stop := serve(t, a)
	<-a.Ready()
	stop()
}
