package machine

import (
	"maps"
	"path"
	"slices"
	"strings"
)

// The limits on the files a Machine places, which the Guest Agent holds to as
// well (ADR 0048): a request outside them is refused at Create.
const (
	maxRunFiles     = 64
	maxRunFileBytes = 1 << 20
	maxRunFileMode  = 0o7777
)

var reservedRunPaths = []string{"/proc", "/sys", "/dev", "/run/mvmd"}

// EnvVar is one environment variable of the entrypoint. The value may be
// secret and is never logged or returned.
type EnvVar struct {
	Name  string
	Value string
}

// PlacedFile is a file the guest places before the entrypoint runs. Content may be
// secret and is never logged or returned.
type PlacedFile struct {
	Path    string
	Content []byte
	Mode    uint32
	UID     uint32
	GID     uint32
}

// PlacedFileInfo is what the Machine reports of a PlacedFile: all but the content.
type PlacedFileInfo struct {
	Path      string
	Mode      uint32
	UID       uint32
	GID       uint32
	SizeBytes uint64
}

// Info returns the file without its content.
func (f PlacedFile) Info() PlacedFileInfo {
	return PlacedFileInfo{Path: f.Path, Mode: f.Mode, UID: f.UID, GID: f.GID, SizeBytes: uint64(len(f.Content))}
}

// validateRun refuses an environment or a set of files the guest would refuse,
// so a bad request never reaches a boot. A message names the field and the
// name or path, never a value or content.
func (s Spec) validateRun() error {
	names := make(map[string]bool, len(s.Env))
	for _, e := range s.Env {
		switch {
		case e.Name == "":
			return fail(ErrInvalid, "env: a name is empty")
		case strings.Contains(e.Name, "="):
			return fail(ErrInvalid, "env: name %q contains \"=\"", e.Name)
		case names[e.Name]:
			return fail(ErrInvalid, "env: name %q is given twice", e.Name)
		}
		names[e.Name] = true
	}
	if len(s.Files) > maxRunFiles {
		return fail(ErrInvalid, "files: %d files, at most %d", len(s.Files), maxRunFiles)
	}
	paths := make(map[string]bool, len(s.Files))
	total := 0
	for _, f := range s.Files {
		switch {
		case !absoluteClean(f.Path):
			return fail(ErrInvalid, "files: path %q is not absolute and clean", f.Path)
		case reservedRunPath(f.Path):
			return fail(ErrInvalid, "files: path %q is under a reserved directory", f.Path)
		case f.Mode > maxRunFileMode:
			return fail(ErrInvalid, "files: path %q has a mode above %#o", f.Path, maxRunFileMode)
		case paths[f.Path]:
			return fail(ErrInvalid, "files: path %q is given twice", f.Path)
		}
		paths[f.Path] = true
		total += len(f.Content)
	}
	if total > maxRunFileBytes {
		return fail(ErrInvalid, "files: %d bytes of content, at most %d", total, maxRunFileBytes)
	}
	return nil
}

// absoluteClean reports whether p is an absolute path in its cleaned form and
// not the root.
func absoluteClean(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && p != "/"
}

func reservedRunPath(p string) bool {
	for _, dir := range reservedRunPaths {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// runOn returns what the guest runs for the Machine on the image: the image's
// entrypoint and command, replaced by the Machine's the way Kubernetes does,
// the image's environment under the Machine's, and the user. An image record
// that holds one joined argument vector has no command to replace by args
// alone, so that is ErrPreconditionFailed.
func (s Spec) runOn(image ImageDigest, run ImageRun) (ExecSpec, string, error) {
	entrypoint, cmd := run.Entrypoint, run.Cmd
	switch {
	case len(s.Command) > 0:
		entrypoint, cmd = s.Command, s.Args
	case len(s.Args) > 0 && run.Joined:
		return ExecSpec{}, "", fail(ErrPreconditionFailed, "image %s was pulled before the node kept its entrypoint and command apart, so args alone cannot replace its command; pull the image again", image)
	case len(s.Args) > 0:
		cmd = s.Args
	}
	env := maps.Clone(run.Env)
	if len(s.Env) > 0 && env == nil {
		env = make(map[string]string, len(s.Env))
	}
	for _, e := range s.Env {
		env[e.Name] = e.Value
	}
	user := s.User
	if user == "" {
		user = run.User
	}
	return ExecSpec{Command: slices.Concat(entrypoint, cmd), Env: env, WorkingDir: run.WorkingDir}, user, nil
}

// cloneNonEmpty copies a slice, keeping every empty one nil so that an empty
// list and an absent one compare equal.
func cloneNonEmpty[T any](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	return slices.Clone(in)
}

func cloneFiles(in []PlacedFile) []PlacedFile {
	out := cloneNonEmpty(in)
	for i := range out {
		out[i].Content = cloneNonEmpty(out[i].Content)
	}
	return out
}

// envNames returns the variables with their values dropped.
func envNames(in []EnvVar) []EnvVar {
	out := cloneNonEmpty(in)
	for i := range out {
		out[i].Value = ""
	}
	return out
}

// withoutContent returns the files with their content dropped.
func withoutContent(in []PlacedFile) []PlacedFile {
	out := cloneNonEmpty(in)
	for i := range out {
		out[i].Content = nil
	}
	return out
}
