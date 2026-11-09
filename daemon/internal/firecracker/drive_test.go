package firecracker_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const dataDrive = "disk_3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"

// configWith makes the VMM answer GET /vm/config with the drives given as
// drive_id and path_on_host pairs.
func configWith(f *fakeAPI, drives ...[2]string) {
	body := `{"drives":[`
	for i, d := range drives {
		if i > 0 {
			body += ","
		}
		body += `{"drive_id":"` + d[0] + `","path_on_host":"` + d[1] + `","is_read_only":false}`
	}
	body += `]}`
	f.answer = func(r request) (int, string) {
		if r.Method == "GET" && r.Path == "/vm/config" {
			return http.StatusOK, body
		}
		return http.StatusNoContent, ""
	}
}

func TestDrive_ReadsTheVMMsConfigurationAndTellsHowTheDriveCameToBe(t *testing.T) {
	tests := []struct {
		name   string
		drives [][2]string
		want   machine.DriveState
	}{
		{"no drive of the disk", [][2]string{{"disk0", "/disk/0"}}, machine.DriveAbsent},
		{"a drive given at boot", [][2]string{{"disk0", "/disk/0"}, {dataDrive, "/disk/2"}}, machine.DriveAtBoot},
		{"a drive added to the running VMM", [][2]string{{"disk0", "/disk/0"}, {dataDrive, "/run/" + dataDrive}}, machine.DriveHotPlugged},
		{"another disk's drive in the run directory", [][2]string{{"disk_other", "/run/disk_other"}}, machine.DriveAbsent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{}
			n.booted(f)
			configWith(f, tt.drives...)
			before := len(f.seen())

			got, err := n.driver.Drive(context.Background(), "m1", "data")

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			sent := since(f, before)
			require.Len(t, sent, 1, "a read, and nothing that changes the VMM")
			assert.Equal(t, request{Method: "GET", Path: "/vm/config"}, sent[0])
		})
	}
}

func TestDrive_ReportsAVMMThatCannotAnswer(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.answer = func(request) (int, string) { return http.StatusBadRequest, `{"fault_message":"not now"}` }

	_, err := n.driver.Drive(context.Background(), "m1", "data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not now")

	configWith(f, [2]string{dataDrive, "/run/" + dataDrive})
	got, err := n.driver.Drive(context.Background(), "m1", "data")
	require.NoError(t, err, "control: the VMM that answers is read")
	assert.Equal(t, machine.DriveHotPlugged, got)
}

func TestDrive_RefusesNoDisk(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	_, err := n.driver.Drive(context.Background(), "m1", "")
	require.Error(t, err)
	assert.Empty(t, since(f, before), "nothing is sent")
}
