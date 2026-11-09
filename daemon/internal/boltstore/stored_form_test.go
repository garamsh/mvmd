package boltstore_test

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// storedBytes reads the raw value of a Machine's record, and of each of its
// exec entries, from a closed database.
func storedBytes(t *testing.T, dir string, id string) (record string, execs map[string]string) {
	t.Helper()
	raw, err := bolt.Open(filepath.Join(dir, "mvmd.db"), 0o600, nil)
	require.NoError(t, err)
	defer raw.Close()
	execs = map[string]string{}
	require.NoError(t, raw.View(func(tx *bolt.Tx) error {
		record = string(tx.Bucket([]byte("machines")).Get([]byte(id)))
		if b := tx.Bucket([]byte("execs")).Bucket([]byte(id)); b != nil {
			return b.ForEach(func(k, v []byte) error { execs[string(k)] = string(v); return nil })
		}
		return nil
	}))
	return record, execs
}

func pausedMachine() *machine.Machine {
	return &machine.Machine{
		ID: "m", State: machine.StatePaused, Spec: legacySpec(), RootDisk: "m-root", RootDiskUUID: "uuid-1",
		User: machine.User{UID: 1001, GID: 1001},
		Network: machine.NetworkIdentity{
			MAC:       "02:00:00:00:00:01",
			Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("fd00::5")},
			Gateways:  []netip.Addr{netip.MustParseAddr("10.0.0.1")},
			Resolvers: []netip.Addr{netip.MustParseAddr("10.0.0.2")},
		},
		FailureReason: "boot failed: boom",
	}
}

// What the store writes is its own format: enums by name, each field under an
// explicit name. These bytes are the format; a change to them is a change to
// what every database holds, and has to be made on purpose.
const (
	storedMachineJSON = `{"ID":"m","Machine":{"ID":"m","State":"paused","Image":"sha256:abc","Resources":{"VCPUs":2,"MemoryBytes":1073741824},"RootDiskBytes":2147483648,"Disks":[{"Disk":"data","ReadOnly":true}],"Policy":{"Egress":[{"Prefix":"10.0.0.0/8","Protocol":"tcp","Ports":[{"First":80,"Last":443}]}],"Ingress":[{"Prefix":"fd00::/8","Protocol":"icmpv6","Ports":null}]},"RootDisk":"m-root","RootDiskUUID":"uuid-1","User":{"UID":1001,"GID":1001},"Network":{"MAC":"02:00:00:00:00:01","Addresses":["10.0.0.5","fd00::5"],"Gateways":["10.0.0.1"],"Resolvers":["10.0.0.2"]},"FailureReason":"boot failed: boom"},"Request":{"ID":"r1","Snapshot":"snap","Spec":{"Image":"sha256:abc","Resources":{"VCPUs":2,"MemoryBytes":1073741824},"RootDiskBytes":2147483648,"Disks":[{"Disk":"data","ReadOnly":true}],"Policy":{"Egress":[{"Prefix":"10.0.0.0/8","Protocol":"tcp","Ports":[{"First":80,"Last":443}]}],"Ingress":[{"Prefix":"fd00::/8","Protocol":"icmpv6","Ports":null}]}},"Failure":null}}`
	storedExecJSON    = `{"Machine":"m","ID":"a","Request":"r","Spec":{"Command":["echo","hi"],"Env":{"K":"v"},"WorkingDir":"/tmp"},"Record":{"ID":"a","Status":"exited","Reason":"","ExitCode":3,"TermSignal":9},"Answered":true}`
)

func TestStore_WritesItsOwnFormat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)
	exec := machine.ExecEntry{
		Machine: "m", ID: "a", Request: "r", Answered: true,
		Spec:   machine.ExecSpec{Command: []string{"echo", "hi"}, Env: map[string]string{"K": "v"}, WorkingDir: "/tmp"},
		Record: machine.ExecRecord{ID: "a", Status: machine.ExecExited, ExitCode: 3, TermSignal: 9},
	}
	require.NoError(t, db.Machines().Save(ctx, machine.Record{
		ID: "m", Machine: pausedMachine(),
		Request: machine.Request{ID: "r1", Snapshot: "snap", Spec: legacySpec()},
		Execs:   []machine.ExecEntry{exec},
	}))
	require.NoError(t, db.Close())

	record, execs := storedBytes(t, dir, "m")
	assert.JSONEq(t, storedMachineJSON, record)
	assert.JSONEq(t, storedExecJSON, execs["a"])
	assert.NotContains(t, record, `"Driver"`, "what Inspect sets is not stored")

	// It reads back as what was saved.
	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, pausedMachine(), got[0].Machine)
	assert.Equal(t, []machine.ExecEntry{exec}, got[0].Execs)
}

// Each state, protocol and exec status is stored under its own name and read
// back as the same value.
func TestStore_EachEnumIsStoredByItsName(t *testing.T) {
	ctx := context.Background()
	states := map[machine.State]string{
		machine.StateCreated: "created", machine.StateRunning: "running", machine.StatePaused: "paused",
		machine.StateStopped: "stopped", machine.StateFaulted: "faulted",
	}
	protocols := map[machine.Protocol]string{
		machine.ProtocolAny: "any", machine.ProtocolTCP: "tcp", machine.ProtocolUDP: "udp",
		machine.ProtocolICMP: "icmp", machine.ProtocolICMPv6: "icmpv6",
	}
	statuses := map[machine.ExecStatus]string{
		machine.ExecNotStarted: "not-started", machine.ExecStarted: "started",
		machine.ExecExited: "exited", machine.ExecCutShort: "cut-short",
	}
	dir := t.TempDir()
	db := open(t, dir)
	for state, name := range states {
		m := pausedMachine()
		m.ID = machine.MachineID(name)
		m.State = state
		require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: m.ID, Machine: m}))
	}
	for protocol := range protocols {
		m := pausedMachine()
		m.ID = machine.MachineID("p-" + protocols[protocol])
		m.Policy.Egress = []machine.NetworkRule{{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Protocol: protocol}}
		require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: m.ID, Machine: m}))
	}
	for status, name := range statuses {
		require.NoError(t, db.Machines().SaveExec(ctx, machine.ExecEntry{Machine: "created", ID: machine.ExecID(name), Record: machine.ExecRecord{Status: status}}))
	}
	require.NoError(t, db.Close())

	for _, name := range states {
		record, _ := storedBytes(t, dir, name)
		assert.Contains(t, record, `"State":"`+name+`"`)
	}
	for _, name := range protocols {
		record, _ := storedBytes(t, dir, "p-"+name)
		assert.Contains(t, record, `"Protocol":"`+name+`"`)
	}
	_, execs := storedBytes(t, dir, "created")
	for _, name := range statuses {
		assert.Contains(t, execs[name], `"Status":"`+name+`"`)
	}

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	byID := map[machine.MachineID]machine.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	for state, name := range states {
		assert.Equal(t, state, byID[machine.MachineID(name)].Machine.State, name)
	}
	for protocol, name := range protocols {
		assert.Equal(t, protocol, byID[machine.MachineID("p-"+name)].Machine.Policy.Egress[0].Protocol, name)
	}
	read := map[machine.ExecID]machine.ExecStatus{}
	for _, e := range byID["created"].Execs {
		read[e.ID] = e.Record.Status
	}
	for status, name := range statuses {
		assert.Equal(t, status, read[machine.ExecID(name)], name)
	}
}

// The numbers the first format wrote mean what they meant then. They are
// pinned here as numbers, not as the constants' current values, so a
// reordering of the constants in package machine leaves what an old database
// says as it was: a decoder that cast the number to the type would read the
// wrong state after one, and fail here. The control is that the names stay put.
func TestStore_TheNumbersOfTheEarlierFormatKeepTheirMeaning(t *testing.T) {
	ctx := context.Background()
	for number, want := range map[string]machine.State{
		"0": machine.StateCreated, "1": machine.StateRunning, "2": machine.StatePaused,
		"3": machine.StateStopped, "4": machine.StateFaulted,
	} {
		t.Run("state "+number, func(t *testing.T) {
			raw := strings.Replace(legacyMachine, `"State":2`, `"State":`+number, 1)
			db := open(t, writeLegacy(t, map[string]string{"m": raw}, nil))
			defer db.Close()
			got, err := db.Machines().Load(ctx)
			require.NoError(t, err)
			require.Empty(t, got[0].Unreadable)
			assert.Equal(t, want, got[0].Machine.State)
		})
	}
	for number, want := range map[string]machine.Protocol{
		"0": machine.ProtocolAny, "1": machine.ProtocolTCP, "2": machine.ProtocolUDP,
		"3": machine.ProtocolICMP, "4": machine.ProtocolICMPv6,
	} {
		t.Run("protocol "+number, func(t *testing.T) {
			raw := strings.Replace(legacyMachine, `"Protocol":1,`, `"Protocol":`+number+`,`, 1)
			db := open(t, writeLegacy(t, map[string]string{"m": raw}, nil))
			defer db.Close()
			got, err := db.Machines().Load(ctx)
			require.NoError(t, err)
			require.Empty(t, got[0].Unreadable)
			assert.Equal(t, want, got[0].Machine.Policy.Egress[0].Protocol)
		})
	}
	for number, want := range map[string]machine.ExecStatus{
		"0": machine.ExecNotStarted, "1": machine.ExecStarted, "2": machine.ExecExited, "3": machine.ExecCutShort,
	} {
		t.Run("exec status "+number, func(t *testing.T) {
			raw := strings.Replace(legacyExecA, `"Status":2`, `"Status":`+number, 1)
			db := open(t, writeLegacy(t, map[string]string{"m": legacyMachine}, map[string]string{"a": raw}))
			defer db.Close()
			got, err := db.Machines().Load(ctx)
			require.NoError(t, err)
			require.Len(t, got[0].Execs, 1)
			assert.Empty(t, got[0].Execs[0].Unreadable)
			assert.Equal(t, want, got[0].Execs[0].Record.Status)
		})
	}
}

// A name or a number the store does not know makes the record unreadable, or
// the exec entry, and nothing is guessed. A known one in the same place is the
// control.
func TestStore_AnUnknownEnumMakesWhatHoldsItUnreadable(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name, from, to string
	}{
		{"an unknown state name", `"State":2`, `"State":"asleep"`},
		{"a state number the earlier format never wrote", `"State":2`, `"State":9`},
		{"no state", `"State":2,`, ``},
		{"an unknown protocol name", `"Protocol":1,`, `"Protocol":"sctp",`},
		{"a protocol number the earlier format never wrote", `"Protocol":1,`, `"Protocol":7,`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(legacyMachine, tt.from, tt.to, 1)
			require.NotEqual(t, legacyMachine, raw, "the setup: the bytes changed")
			db := open(t, writeLegacy(t, map[string]string{"m": raw, "ok": legacyMachine}, nil))
			defer db.Close()
			got, err := db.Machines().Load(ctx)
			require.NoError(t, err)
			require.Len(t, got, 2)
			assert.Equal(t, machine.MachineID("m"), got[0].ID)
			assert.NotEmpty(t, got[0].Unreadable)
			assert.Nil(t, got[0].Machine)
			assert.Empty(t, got[1].Unreadable, "control: the same bytes with a known value are read")
		})
	}

	t.Run("an unknown exec status", func(t *testing.T) {
		bad := strings.Replace(legacyExecA, `"Status":2`, `"Status":"vanished"`, 1)
		db := open(t, writeLegacy(t, map[string]string{"m": legacyMachine}, map[string]string{"a": bad, "b": legacyExecB}))
		defer db.Close()
		got, err := db.Machines().Load(ctx)
		require.NoError(t, err)
		require.Empty(t, got[0].Unreadable, "the Machine is read")
		require.Len(t, got[0].Execs, 2)
		assert.Equal(t, machine.MachineID("m"), got[0].Execs[0].Machine)
		assert.Equal(t, machine.ExecID("a"), got[0].Execs[0].ID)
		assert.NotEmpty(t, got[0].Execs[0].Unreadable)
		assert.Empty(t, got[0].Execs[1].Unreadable, "control: another entry is read")
	})
}

// A record the earlier format wrote is written in the new one when it is
// saved again, and a database may hold both.
func TestStore_ARecordSavedAgainIsWrittenInTheNewFormat(t *testing.T) {
	ctx := context.Background()
	dir := writeLegacy(t, map[string]string{"m": legacyMachine, "failed": legacyFailed}, nil)
	db := open(t, dir)
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Machines().Save(ctx, got[1]))
	require.NoError(t, db.Close())

	record, _ := storedBytes(t, dir, "m")
	assert.JSONEq(t, storedMachineJSON, record, "the earlier bytes became the new ones, with the same meaning")
	failed, _ := storedBytes(t, dir, "failed")
	assert.JSONEq(t, legacyFailed, failed, "a record not saved again is as it was")
}
