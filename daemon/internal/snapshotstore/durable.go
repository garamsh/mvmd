package snapshotstore

import (
	"errors"
	"os"
)

// writeFileSync creates the file with content and syncs it. The file must not
// exist.
func writeFileSync(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// syncFile flushes the file at path.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// syncDir flushes the directory's entries, which a sync of the files in it
// does not (fsync(2)).
func syncDir(path string) error { return syncFile(path) }
