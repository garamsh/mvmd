package snapshotstore

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// recordVersion is the form of record.json this package writes and reads.
const recordVersion = 1

// record is the form a snapshot's record takes on disk. It is this package's:
// every field is named by a tag, so a change in package machine cannot change
// what an existing record means (a renamed field stops this file compiling).
// machine.Snapshot has no field for a network policy, so none is stored.
type record struct {
	Version     int    `json:"version"`
	ID          string `json:"id"`
	Machine     string `json:"machine"`
	Request     string `json:"request"`
	Image       string `json:"image"`
	VCPUs       uint32 `json:"vcpus"`
	MemoryBytes uint64 `json:"memory_bytes"`
	// The ceiling and the boot memory are zero in a record written before
	// they existed.
	CeilingVCPUs       uint32 `json:"ceiling_vcpus"`
	CeilingMemoryBytes uint64 `json:"ceiling_memory_bytes"`
	BootMemoryBytes    uint64 `json:"boot_memory_bytes"`
	RootDiskBytes      uint64 `json:"root_disk_bytes"`
	RootDiskUUID       string `json:"root_disk_uuid"`
	MAC                string `json:"mac"`
	Driver             string `json:"driver"`
	DriverVersion      string `json:"driver_version"`
	CPUModel           string `json:"cpu_model"`
	KernelDigest       string `json:"kernel_digest"`
	// KernelName is empty in a record written before kernels had names.
	KernelName string   `json:"kernel_name"`
	State      []string `json:"state"`
	// Disks is absent from a record written before disks were recorded.
	Disks []recordDisk `json:"disks,omitempty"`
	// CreatedUnixNano is when the snapshot was stored, in nanoseconds since the
	// Unix epoch. It is absent from a record written before it was recorded.
	CreatedUnixNano int64 `json:"created_unix_nano,omitempty"`
	// The run configuration is absent from a record written before it was
	// recorded, and the restored Machine then boots with the image's own. Env
	// values and file content may be secret: they are held in this file, which
	// only the daemon reads.
	Command []string     `json:"command,omitempty"`
	Args    []string     `json:"args,omitempty"`
	Env     []recordEnv  `json:"env,omitempty"`
	RunUser string       `json:"run_user,omitempty"`
	Files   []recordFile `json:"files,omitempty"`
}

type recordEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type recordFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
}

// recordDisk is an attached disk and what its file showed at capture; the
// modification time is in nanoseconds since the Unix epoch.
type recordDisk struct {
	ID          string `json:"id"`
	ReadOnly    bool   `json:"read_only"`
	MountPath   string `json:"mount_path,omitempty"`
	SizeBytes   uint64 `json:"size_bytes"`
	ModUnixNano int64  `json:"mod_unix_nano"`
}

func encodeRecord(s machine.Snapshot, state []string) ([]byte, error) {
	var disks []recordDisk
	for _, d := range s.Disks {
		disks = append(disks, recordDisk{ID: string(d.Disk), ReadOnly: d.ReadOnly, MountPath: d.MountPath, SizeBytes: d.SizeBytes, ModUnixNano: d.ModTime.UnixNano()})
	}
	var env []recordEnv
	for _, e := range s.Spec.Env {
		env = append(env, recordEnv{Name: e.Name, Value: e.Value})
	}
	var files []recordFile
	for _, f := range s.Spec.Files {
		files = append(files, recordFile{Path: f.Path, Content: f.Content, Mode: f.Mode, UID: f.UID, GID: f.GID})
	}
	var created int64
	if !s.CreatedAt.IsZero() {
		created = s.CreatedAt.UnixNano()
	}
	return json.Marshal(record{
		Version:            recordVersion,
		ID:                 string(s.ID),
		Machine:            string(s.Machine),
		Request:            string(s.Request),
		Image:              string(s.Spec.Image),
		VCPUs:              s.Spec.Resources.VCPUs,
		MemoryBytes:        s.Spec.Resources.MemoryBytes,
		CeilingVCPUs:       s.Spec.Ceiling.VCPUs,
		CeilingMemoryBytes: s.Spec.Ceiling.MemoryBytes,
		BootMemoryBytes:    s.Spec.BootMemoryBytes,
		RootDiskBytes:      s.Spec.RootDiskBytes,
		RootDiskUUID:       s.RootDiskUUID,
		MAC:                s.MAC,
		Driver:             s.Compat.Driver,
		DriverVersion:      s.Compat.Version,
		CPUModel:           s.Compat.CPUModel,
		KernelDigest:       s.Compat.KernelDigest,
		KernelName:         s.Compat.KernelName,
		State:              state,
		Disks:              disks,
		CreatedUnixNano:    created,
		Command:            s.Spec.Command,
		Args:               s.Spec.Args,
		Env:                env,
		RunUser:            s.Spec.User,
		Files:              files,
	})
}

// decodeRecord parses record.json. The names of the state files are checked
// to be plain file names, so no record leads a path out of its directory.
func decodeRecord(raw []byte) (machine.Snapshot, []string, error) {
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return machine.Snapshot{}, nil, err
	}
	if r.Version != recordVersion {
		return machine.Snapshot{}, nil, fmt.Errorf("record version %d, this store reads %d", r.Version, recordVersion)
	}
	for _, name := range r.State {
		if err := checkName(name); err != nil {
			return machine.Snapshot{}, nil, err
		}
	}
	resources := machine.Resources{VCPUs: r.VCPUs, MemoryBytes: r.MemoryBytes}
	ceiling := machine.Resources{VCPUs: r.CeilingVCPUs, MemoryBytes: r.CeilingMemoryBytes}.OrElse(resources)
	if r.BootMemoryBytes == 0 {
		r.BootMemoryBytes = resources.MemoryBytes
	}
	var disks []machine.SnapshotDisk
	for _, d := range r.Disks {
		disks = append(disks, machine.SnapshotDisk{Disk: machine.DiskID(d.ID), ReadOnly: d.ReadOnly, MountPath: d.MountPath, SizeBytes: d.SizeBytes, ModTime: time.Unix(0, d.ModUnixNano)})
	}
	var env []machine.EnvVar
	for _, e := range r.Env {
		env = append(env, machine.EnvVar{Name: e.Name, Value: e.Value})
	}
	var files []machine.PlacedFile
	for _, f := range r.Files {
		files = append(files, machine.PlacedFile{Path: f.Path, Content: f.Content, Mode: f.Mode, UID: f.UID, GID: f.GID})
	}
	var created time.Time
	if r.CreatedUnixNano != 0 {
		created = time.Unix(0, r.CreatedUnixNano)
	}
	return machine.Snapshot{
		CreatedAt: created,
		ID:        machine.SnapshotID(r.ID),
		Machine:   machine.MachineID(r.Machine),
		Request:   machine.RequestID(r.Request),
		Spec: machine.SnapshotSpec{
			Image:           machine.ImageDigest(r.Image),
			Resources:       resources,
			Ceiling:         ceiling,
			BootMemoryBytes: r.BootMemoryBytes,
			RootDiskBytes:   r.RootDiskBytes,
			Command:         r.Command,
			Args:            r.Args,
			Env:             env,
			User:            r.RunUser,
			Files:           files,
		},
		RootDiskUUID: r.RootDiskUUID,
		MAC:          r.MAC,
		Compat: machine.SnapshotCompat{
			Driver: r.Driver, Version: r.DriverVersion, CPUModel: r.CPUModel, KernelDigest: r.KernelDigest, KernelName: r.KernelName,
		},
		Disks: disks,
	}, r.State, nil
}

// checkName rejects a name that is not one file name inside a directory.
func checkName(name string) error {
	if name == "" || name == "." || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fmt.Errorf("state file name %q is not a plain file name", name)
	}
	return nil
}
