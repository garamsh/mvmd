// Package boltstore is mvmd's durable State Store on bbolt: one database file
// in the state directory, a bucket for the Machine records, one for their
// exec entries and one for the fencing deadline. A write returns once its transaction is committed and
// the file synced, which is what "acknowledged only after durably recorded"
// needs (runtime-api.md §Fencing deadline).
package boltstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	machinesBucket = []byte("machines")
	deadlineBucket = []byte("deadline")
	deadlineKey    = []byte("deadline")
	// execsBucket holds one bucket per Machine, named for it, of that Machine's
	// exec entries by exec identifier. A database made before it existed has
	// none, and Open adds it.
	execsBucket = []byte("execs")
)

// DB is the open database. Machines and Deadline are the two stores on it.
type DB struct {
	db *bolt.DB
}

// Open opens, or creates, the database file in dir. It fails if another
// process holds the file, rather than waiting for it.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory: %w", err)
	}
	path := filepath.Join(dir, "mvmd.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{machinesBucket, deadlineBucket, execsBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create the buckets of %s: %w", path, err)
	}
	return &DB{db: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Machines returns the store of Machine records, a machine.StateStore.
func (d *DB) Machines() *MachineStore { return &MachineStore{db: d.db} }

// Deadline returns the store of the fencing deadline, a fencing.DeadlineStore.
func (d *DB) Deadline() *DeadlineStore { return &DeadlineStore{db: d.db} }

// update runs fn in a write transaction, committed and synced before it
// returns. A cancelled context stops it before it begins.
func update(ctx context.Context, db *bolt.DB, fn func(*bolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return db.Update(fn)
}
