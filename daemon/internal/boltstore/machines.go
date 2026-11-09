package boltstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// MachineStore keeps the record of each Machine identifier: the Machine, if
// one exists, and the creating call that named it, with the Machine's exec
// entries.
type MachineStore struct {
	db *bolt.DB
}

// The outcome of a failed creating call is stored as the name of its kind and
// its reason.
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
	stored, err := storeMachine(r.Machine)
	if err != nil {
		return fmt.Errorf("record %q: %w", r.ID, err)
	}
	spec, err := storeSpec(r.Request.Spec)
	if err != nil {
		return fmt.Errorf("record %q: %w", r.ID, err)
	}
	j := storedRecord{ID: r.ID, Machine: stored, Request: storedRequest{ID: r.Request.ID, Snapshot: r.Request.Snapshot, Spec: spec}}
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
	entries := make(map[machine.ExecID][]byte, len(r.Execs))
	for _, e := range r.Execs {
		if entries[e.ID], err = marshalExec(e); err != nil {
			return fmt.Errorf("record %q: %w", r.ID, err)
		}
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		if err := tx.Bucket(machinesBucket).Put([]byte(r.ID), raw); err != nil {
			return err
		}
		for id, entry := range entries {
			if err := putExec(tx, r.ID, id, entry); err != nil {
				return err
			}
		}
		return nil
	})
}

// putExec stores one encoded exec entry under the Machine's bucket of them.
func putExec(tx *bolt.Tx, machineID machine.MachineID, id machine.ExecID, entry []byte) error {
	b, err := tx.Bucket(execsBucket).CreateBucketIfNotExists([]byte(machineID))
	if err != nil {
		return err
	}
	return b.Put([]byte(id), entry)
}

// SaveExec implements machine.StateStore.
func (s *MachineStore) SaveExec(ctx context.Context, e machine.ExecEntry) error {
	raw, err := marshalExec(e)
	if err != nil {
		return err
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		if tx.Bucket(machinesBucket).Get([]byte(e.Machine)) == nil {
			return fmt.Errorf("no record of Machine %q", e.Machine)
		}
		return putExec(tx, e.Machine, e.ID, raw)
	})
}

// Remove implements machine.StateStore.
func (s *MachineStore) Remove(ctx context.Context, id machine.MachineID) error {
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		if err := tx.Bucket(machinesBucket).Delete([]byte(id)); err != nil {
			return err
		}
		if err := tx.Bucket(execsBucket).DeleteBucket([]byte(id)); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		return nil
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
			if err == nil {
				r.Execs, err = loadExecs(tx, r.ID)
			}
			if err != nil {
				r = machine.Record{ID: machine.MachineID(k), Unreadable: err.Error()}
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

// loadExecs reads the Machine's exec entries, ordered by exec identifier.
func loadExecs(tx *bolt.Tx, id machine.MachineID) ([]machine.ExecEntry, error) {
	b := tx.Bucket(execsBucket).Bucket([]byte(id))
	if b == nil {
		return nil, nil
	}
	var out []machine.ExecEntry
	err := b.ForEach(func(k, v []byte) error {
		e, err := decodeExec(v)
		if err != nil {
			// The bucket names say whose entry it is; nothing else is known.
			e = machine.ExecEntry{Machine: id, ID: machine.ExecID(k), Unreadable: fmt.Sprintf("decode the exec entry: %v", err)}
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func marshalExec(e machine.ExecEntry) ([]byte, error) {
	stored, err := storeExec(e)
	if err != nil {
		return nil, fmt.Errorf("exec %q: %w", e.ID, err)
	}
	return json.Marshal(stored)
}

func decodeExec(raw []byte) (machine.ExecEntry, error) {
	var j storedExec
	if err := json.Unmarshal(raw, &j); err != nil {
		return machine.ExecEntry{}, err
	}
	return j.load()
}

func decode(raw []byte) (machine.Record, error) {
	var j storedRecord
	if err := json.Unmarshal(raw, &j); err != nil {
		return machine.Record{}, fmt.Errorf("decode the record: %w", err)
	}
	m, err := j.Machine.load()
	if err != nil {
		return machine.Record{}, fmt.Errorf("decode the record: %w", err)
	}
	spec, err := j.Request.Spec.load()
	if err != nil {
		return machine.Record{}, fmt.Errorf("decode the record: %w", err)
	}
	r := machine.Record{ID: j.ID, Machine: m, Request: machine.Request{ID: j.Request.ID, Snapshot: j.Request.Snapshot, Spec: spec}}
	if f := j.Request.Failure; f != nil {
		kind, ok := kinds[f.Kind]
		if !ok {
			return machine.Record{}, fmt.Errorf("the record names the unknown outcome kind %q", f.Kind)
		}
		r.Request.Failure = machine.NewError(kind, "%s", f.Reason)
	}
	return r, nil
}
