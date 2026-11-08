package boltstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
)

// DeadlineStore keeps the fencing deadline record.
type DeadlineStore struct {
	db *bolt.DB
}

// deadlineJSON is the stored form of fencing.Record.
type deadlineJSON struct {
	Expiry  int64
	BootID  string
	Length  time.Duration
	Renewal uint64
}

// Load implements fencing.DeadlineStore.
func (s *DeadlineStore) Load(ctx context.Context) (fencing.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return fencing.Record{}, false, err
	}
	var raw []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(deadlineBucket).Get(deadlineKey); v != nil {
			raw = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil || raw == nil {
		return fencing.Record{}, false, err
	}
	var j deadlineJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return fencing.Record{}, false, fmt.Errorf("decode the deadline record: %w", err)
	}
	return fencing.Record{Expiry: fencing.BootTime(j.Expiry), BootID: j.BootID, Length: j.Length, Renewal: j.Renewal}, true, nil
}

// Save implements fencing.DeadlineStore.
func (s *DeadlineStore) Save(ctx context.Context, r fencing.Record) error {
	raw, err := json.Marshal(deadlineJSON{Expiry: int64(r.Expiry), BootID: r.BootID, Length: r.Length, Renewal: r.Renewal})
	if err != nil {
		return err
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		return tx.Bucket(deadlineBucket).Put(deadlineKey, raw)
	})
}
