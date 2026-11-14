package boltstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// fencePrefix starts the key of a Machine's own fence in the deadline bucket
// (ADR 0064). The node's record has a key that does not start with it.
var fencePrefix = []byte("fence/")

// FenceStore keeps the fences of the Machines created with one.
type FenceStore struct {
	db *bolt.DB
}

func fenceKey(id machine.MachineID) []byte {
	return append(append([]byte(nil), fencePrefix...), id...)
}

// Load implements fencing.FenceStore.
func (s *FenceStore) Load(ctx context.Context) (map[machine.MachineID]fencing.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[machine.MachineID]fencing.Record{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(deadlineBucket).Cursor()
		for k, v := c.Seek(fencePrefix); k != nil && bytes.HasPrefix(k, fencePrefix); k, v = c.Next() {
			var j deadlineJSON
			if err := json.Unmarshal(v, &j); err != nil {
				return fmt.Errorf("decode the fence record of %q: %w", k[len(fencePrefix):], err)
			}
			out[machine.MachineID(k[len(fencePrefix):])] = fencing.Record{Expiry: fencing.BootTime(j.Expiry), BootID: j.BootID, Length: j.Length, Renewal: j.Renewal}
		}
		return nil
	})
	return out, err
}

// Save implements fencing.FenceStore.
func (s *FenceStore) Save(ctx context.Context, id machine.MachineID, r fencing.Record) error {
	raw, err := json.Marshal(deadlineJSON{Expiry: int64(r.Expiry), BootID: r.BootID, Length: r.Length, Renewal: r.Renewal})
	if err != nil {
		return err
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		return tx.Bucket(deadlineBucket).Put(fenceKey(id), raw)
	})
}

// Delete implements fencing.FenceStore.
func (s *FenceStore) Delete(ctx context.Context, id machine.MachineID) error {
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		return tx.Bucket(deadlineBucket).Delete(fenceKey(id))
	})
}
