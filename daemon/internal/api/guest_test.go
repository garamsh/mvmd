package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// execer is a machine.Service whose Exec answers a fixed record.
type execer struct {
	machine.Service
	record machine.ExecRecord
}

func (e *execer) Exec(context.Context, machine.MachineID, machine.ExecID, machine.RequestID, machine.ExecSpec) (machine.ExecRecord, error) {
	return e.record, nil
}

// The time the guest stamped goes to the client as it is kept, and a record
// with none goes with none: the API never fills one in.
func TestExec_ReportsWhenTheCommandEndedAndNothingWhereItIsUnknown(t *testing.T) {
	ended := time.Date(2026, 11, 11, 12, 34, 56, 123456789, time.UTC)
	tests := []struct {
		name   string
		record machine.ExecRecord
		want   *time.Time
	}{
		{"a time", machine.ExecRecord{ID: "e", Status: machine.ExecExited, EndedAt: ended}, &ended},
		{"no time", machine.ExecRecord{ID: "e", Status: machine.ExecExited}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &execer{record: tt.record}

			resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).Exec(context.Background(), &v1alpha2.ExecRequest{ExecId: "e", RequestId: "r", MachineId: "m", Command: []string{"x"}})

			require.NoError(t, err)
			if tt.want == nil {
				assert.Nil(t, resp.GetRecord().GetEndedAt())
				return
			}
			require.NotNil(t, resp.GetRecord().GetEndedAt())
			assert.True(t, tt.want.Equal(resp.GetRecord().GetEndedAt().AsTime()), "%v", resp.GetRecord().GetEndedAt().AsTime())
		})
	}
}

func TestExec_ReportsTheSignalThatEndedTheCommand(t *testing.T) {
	tests := []struct {
		name   string
		record machine.ExecRecord
		exit   int32
		signal int32
	}{
		{"a command a signal ended", machine.ExecRecord{ID: "e", Status: machine.ExecExited, TermSignal: 9}, 0, 9},
		{"a command that exited", machine.ExecRecord{ID: "e", Status: machine.ExecExited, ExitCode: 3}, 3, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &execer{record: tt.record}

			resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).Exec(context.Background(), &v1alpha2.ExecRequest{ExecId: "e", RequestId: "r", MachineId: "m", Command: []string{"x"}})

			require.NoError(t, err)
			assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_EXITED, resp.GetRecord().GetStatus())
			assert.Equal(t, tt.exit, resp.GetRecord().GetExitCode())
			assert.Equal(t, tt.signal, resp.GetRecord().GetTermSignal())
		})
	}
}
