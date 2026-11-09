package imagestore

import (
	"errors"
	"os"
)

// noFault is the fault of a step that is never made to fail.
func noFault(string) error { return nil }

// replaceFile makes the file hold content in one rename: the content is
// written and synced beside it first, so a crash leaves the old file or the
// new one. The caller syncs the directory.
func replaceFile(path string, content []byte, fault func(step string) error) error {
	tmp := path + ".tmp"
	if err := fault("write-index"); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := fault("sync-index"); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := fault("replace-index"); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

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
