package boltstore

import (
	"context"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// MachineStore keeps the record of each Machine identifier: the Machine, if
// one exists, and the creating call that named it.
type MachineStore struct {
	db *bolt.DB
}

// recordJSON is the stored form of machine.Record. The outcome of a failed
// creating call is kept as the name of its kind and its reason.
type recordJSON struct {
	ID      machine.MachineID
	Machine *machine.Machine
	Request requestJSON
}

type requestJSON struct {
	ID       machine.RequestID
	Snapshot machine.SnapshotID
	Spec     machine.Spec
	Failure  *failureJSON
}

type failureJSON struct {
	Kind   string
	Reason string
}

var kinds = map[string]error{
	"invalid":      machine.ErrInvalid,
	"not-found":    machine.ErrNotFound,
	"conflict":     machine.ErrConflict,
	"precondition": machine.ErrPreconditionFailed,
	"unsupported":  machine.ErrUnsupported,
	"failed":       machine.ErrFailed,
}

func kindName(kind error) (string, bool) {
	for name, k := range kinds {
		if k == kind {
			return name, true
		}
	}
	return "", false
}

// Save implements machine.StateStore.
func (s *MachineStore) Save(ctx context.Context, r machine.Record) error {
	j := recordJSON{ID: r.ID, Machine: r.Machine, Request: requestJSON{ID: r.Request.ID, Snapshot: r.Request.Snapshot, Spec: r.Request.Spec}}
	if f := r.Request.Failure; f != nil {
		name, ok := kindName(f.Kind)
		if !ok {
			return fmt.Errorf("record %q: unknown outcome kind %v", r.ID, f.Kind)
		}
		j.Request.Failure = &failureJSON{Kind: name, Reason: f.Reason}
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		return tx.Bucket(machinesBucket).Put([]byte(r.ID), raw)
	})
}

// Remove implements machine.StateStore.
func (s *MachineStore) Remove(ctx context.Context, id machine.MachineID) error {
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		return tx.Bucket(machinesBucket).Delete([]byte(id))
	})
}

// Load implements machine.StateStore. A record that cannot be decoded is
// returned under its key with Unreadable set to the reason.
func (s *MachineStore) Load(ctx context.Context) ([]machine.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []machine.Record
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(machinesBucket).ForEach(func(k, v []byte) error {
			r, err := decode(v)
			if err != nil {
				r = machine.Record{ID: machine.MachineID(k), Unreadable: err.Error()}
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func decode(raw []byte) (machine.Record, error) {
	var j recordJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return machine.Record{}, fmt.Errorf("decode the record: %w", err)
	}
	r := machine.Record{ID: j.ID, Machine: j.Machine, Request: machine.Request{ID: j.Request.ID, Snapshot: j.Request.Snapshot, Spec: j.Request.Spec}}
	if f := j.Request.Failure; f != nil {
		kind, ok := kinds[f.Kind]
		if !ok {
			return machine.Record{}, fmt.Errorf("the record names the unknown outcome kind %q", f.Kind)
		}
		r.Request.Failure = machine.NewError(kind, "%s", f.Reason)
	}
	return r, nil
}
