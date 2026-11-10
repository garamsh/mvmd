package snapshotstore

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// recordVersion is the form of record.json this package writes and reads.
const recordVersion = 1

// record is the form a snapshot's record takes on disk. It is this package's:
// every field is named by a tag, so a change in package machine cannot change
// what an existing record means (a renamed field stops this file compiling).
// machine.Snapshot has no field for a network policy or an attached disk, so
// neither is stored.
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
	CeilingVCPUs       uint32   `json:"ceiling_vcpus"`
	CeilingMemoryBytes uint64   `json:"ceiling_memory_bytes"`
	BootMemoryBytes    uint64   `json:"boot_memory_bytes"`
	RootDiskBytes      uint64   `json:"root_disk_bytes"`
	RootDiskUUID       string   `json:"root_disk_uuid"`
	MAC                string   `json:"mac"`
	Driver             string   `json:"driver"`
	DriverVersion      string   `json:"driver_version"`
	CPUModel           string   `json:"cpu_model"`
	KernelDigest       string   `json:"kernel_digest"`
	State              []string `json:"state"`
}

func encodeRecord(s machine.Snapshot, state []string) ([]byte, error) {
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
		State:              state,
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
	return machine.Snapshot{
		ID:      machine.SnapshotID(r.ID),
		Machine: machine.MachineID(r.Machine),
		Request: machine.RequestID(r.Request),
		Spec: machine.SnapshotSpec{
			Image:           machine.ImageDigest(r.Image),
			Resources:       resources,
			Ceiling:         ceiling,
			BootMemoryBytes: r.BootMemoryBytes,
			RootDiskBytes:   r.RootDiskBytes,
		},
		RootDiskUUID: r.RootDiskUUID,
		MAC:          r.MAC,
		Compat: machine.SnapshotCompat{
			Driver: r.Driver, Version: r.DriverVersion, CPUModel: r.CPUModel, KernelDigest: r.KernelDigest,
		},
	}, r.State, nil
}

// checkName rejects a name that is not one file name inside a directory.
func checkName(name string) error {
	if name == "" || name == "." || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fmt.Errorf("state file name %q is not a plain file name", name)
	}
	return nil
}
