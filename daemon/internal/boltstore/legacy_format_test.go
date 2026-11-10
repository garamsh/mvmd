package boltstore_test

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The bytes below were written by the store when it wrote the lifecycle's own
// types (before PR for #74): enums as their numbers, fields under their Go
// names, the Machine's Spec flattened into it. A database holding them must
// keep reading the same.
const (
	legacyMachine = `{"ID":"m","Machine":{"ID":"m","State":2,"Image":"sha256:abc","Resources":{"VCPUs":2,"MemoryBytes":1073741824},"RootDiskBytes":2147483648,"Disks":[{"Disk":"data","ReadOnly":true}],"Policy":{"Egress":[{"Prefix":"10.0.0.0/8","Protocol":1,"Ports":[{"First":80,"Last":443}]}],"Ingress":[{"Prefix":"fd00::/8","Protocol":4,"Ports":null}]},"RootDisk":"m-root","RootDiskUUID":"uuid-1","User":{"UID":1001,"GID":1001},"Network":{"MAC":"02:00:00:00:00:01","Addresses":["10.0.0.5","fd00::5"],"Gateways":["10.0.0.1"],"Resolvers":["10.0.0.2"]},"FailureReason":"boot failed: boom","Driver":{"Name":"","Version":"","CPUModel":"","Capabilities":null}},"Request":{"ID":"r1","Snapshot":"snap","Spec":{"Image":"sha256:abc","Resources":{"VCPUs":2,"MemoryBytes":1073741824},"RootDiskBytes":2147483648,"Disks":[{"Disk":"data","ReadOnly":true}],"Policy":{"Egress":[{"Prefix":"10.0.0.0/8","Protocol":1,"Ports":[{"First":80,"Last":443}]}],"Ingress":[{"Prefix":"fd00::/8","Protocol":4,"Ports":null}]}},"Failure":null}}`
	legacyFailed  = `{"ID":"failed","Machine":null,"Request":{"ID":"r2","Snapshot":"","Spec":{"Image":"sha256:abc","Resources":{"VCPUs":2,"MemoryBytes":1073741824},"RootDiskBytes":2147483648,"Disks":[{"Disk":"data","ReadOnly":true}],"Policy":{"Egress":[{"Prefix":"10.0.0.0/8","Protocol":1,"Ports":[{"First":80,"Last":443}]}],"Ingress":[{"Prefix":"fd00::/8","Protocol":4,"Ports":null}]}},"Failure":{"Kind":"failed","Reason":"create root disk: boom"}}}`
	legacyExecA   = `{"Machine":"m","ID":"a","Request":"r","Spec":{"Command":["echo","hi"],"Env":{"K":"v"},"WorkingDir":"/tmp"},"Record":{"ID":"a","Status":2,"Reason":"","ExitCode":3,"TermSignal":9},"Answered":true,"Unreadable":""}`
	legacyExecB   = `{"Machine":"m","ID":"b","Request":"r2","Spec":{"Command":["sleep"],"Env":null,"WorkingDir":""},"Record":{"ID":"","Status":0,"Reason":"","ExitCode":0,"TermSignal":0},"Answered":false,"Unreadable":""}`
	legacyExecC   = `{"Machine":"m","ID":"c","Request":"r3","Spec":{"Command":["x"],"Env":null,"WorkingDir":""},"Record":{"ID":"c","Status":3,"Reason":"ended","ExitCode":0,"TermSignal":0},"Answered":true,"Unreadable":""}`
)

func legacySpec() machine.Spec {
	return machine.Spec{
		Image:         "sha256:abc",
		Resources:     machine.Resources{VCPUs: 2, MemoryBytes: 1 << 30},
		RootDiskBytes: 2 << 30,
		Disks:         []machine.DiskAttachment{{Disk: "data", ReadOnly: true}},
		Policy: machine.NetworkPolicy{
			Egress:  []machine.NetworkRule{{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Protocol: machine.ProtocolTCP, Ports: []machine.PortRange{{First: 80, Last: 443}}}},
			Ingress: []machine.NetworkRule{{Prefix: netip.MustParsePrefix("fd00::/8"), Protocol: machine.ProtocolICMPv6}},
		},
	}
}

// writeLegacy makes a database file holding the given raw values, as the
// earlier store left it.
func writeLegacy(t *testing.T, machines map[string]string, execs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	db, err := boltstore.Open(dir)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	raw, err := bolt.Open(filepath.Join(dir, "mvmd.db"), 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
		for k, v := range machines {
			if err := tx.Bucket([]byte("machines")).Put([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		for k, v := range execs {
			b, err := tx.Bucket([]byte("execs")).CreateBucketIfNotExists([]byte("m"))
			if err != nil {
				return err
			}
			if err := b.Put([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, raw.Close())
	return dir
}

// A database written by the earlier format reads back as the records it was
// made from.
func TestStore_ARecordWrittenByTheEarlierFormatStillReads(t *testing.T) {
	ctx := context.Background()
	dir := writeLegacy(t,
		map[string]string{"m": legacyMachine, "failed": legacyFailed},
		map[string]string{"a": legacyExecA, "b": legacyExecB, "c": legacyExecC})
	db := open(t, dir)
	defer db.Close()

	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, machine.Record{
		ID: "failed",
		Request: machine.Request{
			ID: "r2", Spec: legacySpec(),
			Failure: machine.NewError(machine.ErrFailed, "create root disk: boom"),
		},
	}, got[0])

	// A record from before the ceiling and the boot memory existed reads back
	// with the ceiling the resources are, and booted with the memory.
	wantSpec := legacySpec()
	wantSpec.Ceiling = wantSpec.Resources
	want := machine.Machine{
		BootMemoryBytes: wantSpec.Resources.MemoryBytes,
		ID:              "m", State: machine.StatePaused, Spec: wantSpec, RootDisk: "m-root", RootDiskUUID: "uuid-1",
		User: machine.User{UID: 1001, GID: 1001},
		Network: machine.NetworkIdentity{
			MAC:       "02:00:00:00:00:01",
			Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("fd00::5")},
			Gateways:  []netip.Addr{netip.MustParseAddr("10.0.0.1")},
			Resolvers: []netip.Addr{netip.MustParseAddr("10.0.0.2")},
		},
		FailureReason: "boot failed: boom",
	}
	assert.Equal(t, "m", string(got[1].ID))
	assert.Equal(t, &want, got[1].Machine)
	assert.Equal(t, machine.Request{ID: "r1", Snapshot: "snap", Spec: legacySpec()}, got[1].Request)
	assert.Equal(t, []machine.ExecEntry{
		{
			Machine: "m", ID: "a", Request: "r", Answered: true,
			Spec:   machine.ExecSpec{Command: []string{"echo", "hi"}, Env: map[string]string{"K": "v"}, WorkingDir: "/tmp"},
			Record: machine.ExecRecord{ID: "a", Status: machine.ExecExited, ExitCode: 3, TermSignal: 9},
		},
		{Machine: "m", ID: "b", Request: "r2", Spec: machine.ExecSpec{Command: []string{"sleep"}}},
		{
			Machine: "m", ID: "c", Request: "r3", Answered: true,
			Spec:   machine.ExecSpec{Command: []string{"x"}},
			Record: machine.ExecRecord{ID: "c", Status: machine.ExecCutShort, Reason: "ended"},
		},
	}, got[1].Execs)
}
