package boltstore

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Backup writes a consistent copy of the database of the state directory dir to
// the file dest, which it does not overwrite, and returns the bytes written
// (ADR 0071). It opens the database read-only and makes no change to it, and
// it fails after a second, as Open does, while another process holds the file:
// a running mvmd holds it, so the copy is made with mvmd stopped. The copy is
// made beside dest and renamed into place after it is synced, so a failure
// leaves no partial file named dest.
func Backup(dir, dest string) (int64, error) {
	path := filepath.Join(dir, "mvmd.db")
	if _, err := os.Stat(dest); err == nil {
		return 0, fmt.Errorf("%s exists and is not overwritten", dest)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".mvmd-backup-")
	if err != nil {
		return 0, fmt.Errorf("make the copy beside %s: %w", dest, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	var n int64
	if err := db.View(func(tx *bolt.Tx) error {
		var err error
		n, err = tx.WriteTo(tmp)
		return err
	}); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("copy %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Link(tmp.Name(), dest); err != nil {
		return 0, fmt.Errorf("put the copy at %s: %w", dest, err)
	}
	return n, nil
}
