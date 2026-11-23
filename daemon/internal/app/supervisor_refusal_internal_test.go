package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// listing is the Machines' supervisors with a fixed list of the live ones.
type listing struct {
	fencing.Supervisors
	live []fencing.Supervisor
}

func (l listing) Live(context.Context) ([]fencing.Supervisor, error) { return l.live, nil }

// A supervisor this mvmd does not speak to is found before anything is served or changed: Serve
// returns the refusal, opens no socket, and the control is the same node over supervisors it
// speaks to (ADR 0070).
func TestServe_RefusesBeforeServingWhenASupervisorIsNotSpokenTo(t *testing.T) {
	cfg := config(t)
	a, err := New(cfg)
	require.NoError(t, err)
	a.supervisors = listing{Supervisors: a.supervisors, live: []fencing.Supervisor{{Machine: "m", APIVersion: "v2"}}}

	err = a.Serve(context.Background())

	var incompatible *supervision.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, "m", string(incompatible.Machine))
	assert.NoFileExists(t, cfg.Socket.Path, "nothing was served")
	select {
	case <-a.Ready():
		t.Fatal("the daemon became ready")
	default:
	}

	cfg = config(t)
	a, err = New(cfg)
	require.NoError(t, err)
	a.supervisors = listing{Supervisors: a.supervisors, live: []fencing.Supervisor{{Machine: "m"}}}
	stop := serve(t, a)
	<-a.Ready()
	stop()
}
