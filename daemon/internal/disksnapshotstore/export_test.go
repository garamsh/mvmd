package disksnapshotstore

import "github.com/garamsh/mvmd/daemon/internal/sparsecopy"

// WithFault makes the store ask fault before each step of Put and Remove.
func (s *Store) WithFault(fault func(step string) error) { s.fault = fault }

// WithClone replaces the clone Put tries, to stand for a file system with
// reflinks or one without.
func (s *Store) WithClone(clone sparsecopy.Cloner) { s.clone = clone }
