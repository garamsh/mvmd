package imagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The media types and annotations of a MachineImage. At stage v1 the format is
// node-local and published to no registry (ADR 0007).
const (
	ociManifestType      = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType         = "application/vnd.oci.image.index.v1+json"
	machineImageType     = "application/vnd.mvmd.machineimage.v1"
	machineImageConfig   = "application/vnd.mvmd.machineimage.config.v1+json"
	machineImageFilesys  = "application/vnd.mvmd.machineimage.erofs.v1"
	verityAnnotation     = "io.mvmd.fsverity.digest"
	pulledByAnnotation   = "io.mvmd.image.digest"
	createdAnnotation    = "io.mvmd.image.created"
	layoutVersionContent = `{"imageLayoutVersion":"1.0.0"}`
)

var hexForm = regexp.MustCompile(`^[0-9a-f]{64}$`)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	ArtifactType  string       `json:"artifactType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

// config is the config blob of a MachineImage: what the image runs, the
// converter it came from and its filesystem's UUID. Required kernel
// conditions are none at stage v1.
type config struct {
	Architecture string   `json:"architecture"`
	Converter    string   `json:"converter"`
	UUID         string   `json:"uuid"`
	Run          runSpec  `json:"run"`
	Kernel       []string `json:"requiredKernelConditions"`
}

// splitForm is the Form of a run written with the entrypoint, the command and
// the user apart. A run without it is the old form: one joined vector in
// Command.
const splitForm = 2

type runSpec struct {
	Form       int               `json:"form,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	User       string            `json:"user,omitempty"`
}

func toRunSpec(r machine.ImageRun) runSpec {
	return runSpec{Form: splitForm, Entrypoint: r.Entrypoint, Cmd: r.Cmd, Env: r.Env, WorkingDir: r.WorkingDir, User: r.User}
}

// run reads the record's run; an old-form record is its joined vector as the
// entrypoint, marked.
func (r runSpec) run() machine.ImageRun {
	if r.Form < splitForm {
		return machine.ImageRun{Entrypoint: r.Command, Env: r.Env, WorkingDir: r.WorkingDir}
	}
	return machine.ImageRun{Entrypoint: r.Entrypoint, Cmd: r.Cmd, Env: r.Env, WorkingDir: r.WorkingDir, User: r.User}
}

func (s *Store) indexPath() string        { return filepath.Join(s.dir, "index.json") }
func (s *Store) blobsDir() string         { return filepath.Join(s.dir, "blobs", "sha256") }
func (s *Store) stagingDir() string       { return filepath.Join(s.dir, "staging") }
func (s *Store) blobPath(h string) string { return filepath.Join(s.blobsDir(), h) }

// readIndex returns the index the cache lists images by. A missing or
// malformed index.json is an error: the layout has one from New on.
func (s *Store) readIndex() (index, error) {
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		return index{}, err
	}
	var idx index
	if err := json.Unmarshal(b, &idx); err != nil {
		return index{}, fmt.Errorf("decode index.json: %w", err)
	}
	return idx, nil
}

// readBlob returns the blob of the descriptor after checking its size and
// SHA-256 against the descriptor, which is what makes a manifest or a config
// trustworthy (ADR 0008).
func (s *Store) readBlob(d descriptor, into any) error {
	h, err := hexOf(d.Digest)
	if err != nil {
		return err
	}
	f, err := os.Open(s.blobPath(h))
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, d.Size+1))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) != d.Size || hex.EncodeToString(sum[:]) != h {
		return fmt.Errorf("blob %s does not match its content address", d.Digest)
	}
	return json.Unmarshal(b, into)
}

// hexOf returns the encoded part of a sha256 digest, and so a name that is
// safe to join to a path.
func hexOf(digest string) (string, error) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !hexForm.MatchString(h) {
		return "", fmt.Errorf("%q is not a sha256 digest", digest)
	}
	return h, nil
}

// find returns the index entry of the image pulled by the digest.
func (idx index) find(image machine.ImageDigest) (descriptor, bool) {
	for _, d := range idx.Manifests {
		if d.Annotations[pulledByAnnotation] == string(image) {
			return d, true
		}
	}
	return descriptor{}, false
}

// reaches returns the manifest of a listed image and the digests it reaches:
// the manifest, its config and its layers.
func (s *Store) reaches(d descriptor) (manifest, []string, error) {
	var m manifest
	if err := s.readBlob(d, &m); err != nil {
		return manifest{}, nil, fmt.Errorf("read the manifest %s: %w", d.Digest, err)
	}
	digests := []string{d.Digest, m.Config.Digest}
	for _, l := range m.Layers {
		digests = append(digests, l.Digest)
	}
	return m, digests, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
