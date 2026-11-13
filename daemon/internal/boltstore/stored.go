package boltstore

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The types below are the form the database holds. They are this package's:
// every field is named by a tag, every enum by a name this file defines, and
// nothing in the stored bytes follows a lifecycle type's field names or a
// constant's position, so a change in package machine cannot change what an
// existing database means (a renamed field stops this file compiling). The
// field names are the ones the first format used, so that what it wrote still
// reads.

type storedRecord struct {
	ID      machine.MachineID `json:"ID"`
	Machine *storedMachine    `json:"Machine"`
	Request storedRequest     `json:"Request"`
}

type storedMachine struct {
	ID    machine.MachineID `json:"ID"`
	State json.RawMessage   `json:"State"`
	// storedSpec is flattened into the Machine, as the first format had it.
	storedSpec
	RootDisk      machine.DiskID `json:"RootDisk"`
	RootDiskUUID  string         `json:"RootDiskUUID"`
	User          storedUser     `json:"User"`
	Network       storedNetwork  `json:"Network"`
	FailureReason string         `json:"FailureReason"`
	// BootMemoryBytes is zero in a record written before it existed.
	BootMemoryBytes uint64 `json:"BootMemoryBytes"`
	// KernelDigest is empty in a record written before it existed.
	KernelDigest string `json:"KernelDigest"`
}

type storedSpec struct {
	Image     machine.ImageDigest `json:"Image"`
	Resources storedResources     `json:"Resources"`
	// Ceiling is zero in a record written before the ceiling existed, and in a
	// request that set none.
	Ceiling       storedResources    `json:"Ceiling"`
	RootDiskBytes uint64             `json:"RootDiskBytes"`
	Disks         []storedAttachment `json:"Disks"`
	Policy        storedPolicy       `json:"Policy"`
	// Kernel is empty in a record written before kernels had names.
	Kernel string `json:"Kernel"`
	// The run configuration is absent in a record written before it existed,
	// which boots with the image's own. Env values and file content may be
	// secret: they are held here, in the database file only the daemon reads.
	Command []string     `json:"Command,omitempty"`
	Args    []string     `json:"Args,omitempty"`
	Env     []storedEnv  `json:"Env,omitempty"`
	User    string       `json:"RunUser,omitempty"`
	Files   []storedFile `json:"Files,omitempty"`
}

type storedEnv struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type storedFile struct {
	Path    string `json:"Path"`
	Content []byte `json:"Content"`
	Mode    uint32 `json:"Mode"`
	UID     uint32 `json:"UID"`
	GID     uint32 `json:"GID"`
}

type storedResources struct {
	VCPUs       uint32 `json:"VCPUs"`
	MemoryBytes uint64 `json:"MemoryBytes"`
}

type storedAttachment struct {
	Disk      machine.DiskID `json:"Disk"`
	ReadOnly  bool           `json:"ReadOnly"`
	MountPath string         `json:"MountPath,omitempty"`
}

type storedPolicy struct {
	Egress  []storedRule `json:"Egress"`
	Ingress []storedRule `json:"Ingress"`
}

type storedRule struct {
	Prefix   netip.Prefix    `json:"Prefix"`
	Protocol json.RawMessage `json:"Protocol"`
	Ports    []storedPorts   `json:"Ports"`
}

type storedPorts struct {
	First uint16 `json:"First"`
	Last  uint16 `json:"Last"`
}

type storedUser struct {
	UID uint32 `json:"UID"`
	GID uint32 `json:"GID"`
}

type storedNetwork struct {
	MAC       string       `json:"MAC"`
	Addresses []netip.Addr `json:"Addresses"`
	Gateways  []netip.Addr `json:"Gateways"`
	Resolvers []netip.Addr `json:"Resolvers"`
}

type storedRequest struct {
	ID       machine.RequestID  `json:"ID"`
	Snapshot machine.SnapshotID `json:"Snapshot"`
	Spec     storedSpec         `json:"Spec"`
	Failure  *failureJSON       `json:"Failure"`
}

type failureJSON struct {
	Kind   string `json:"Kind"`
	Reason string `json:"Reason"`
}

type storedExec struct {
	Machine  machine.MachineID `json:"Machine"`
	ID       machine.ExecID    `json:"ID"`
	Request  machine.RequestID `json:"Request"`
	Spec     storedExecSpec    `json:"Spec"`
	Record   storedExecRecord  `json:"Record"`
	Answered bool              `json:"Answered"`
}

type storedExecSpec struct {
	Command    []string          `json:"Command"`
	Env        map[string]string `json:"Env"`
	WorkingDir string            `json:"WorkingDir"`
}

type storedExecRecord struct {
	ID         machine.ExecID  `json:"ID"`
	Status     json.RawMessage `json:"Status"`
	Reason     string          `json:"Reason"`
	ExitCode   int32           `json:"ExitCode"`
	TermSignal int32           `json:"TermSignal"`
	EndedAt    time.Time       `json:"EndedAt,omitzero"`
}

// enum is the stored names of one lifecycle enum. A value is written as its
// name. A name read that is not one of these is an error, and so is a number
// that the first format could not have written: the record is then unreadable
// and nothing is guessed. legacy is what each number the first format wrote
// meant; it is written out here, not taken from the constants, so a reordering
// of them leaves it as it was.
type enum[T comparable] struct {
	what   string
	names  map[T]string
	legacy map[int]T
}

func (e enum[T]) encode(v T) (json.RawMessage, error) {
	name, ok := e.names[v]
	if !ok {
		return nil, fmt.Errorf("%s %v has no stored name", e.what, v)
	}
	return json.Marshal(name)
}

func (e enum[T]) decode(raw json.RawMessage) (T, error) {
	var zero T
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		for v, n := range e.names {
			if n == name {
				return v, nil
			}
		}
		return zero, fmt.Errorf("the record names the unknown %s %q", e.what, name)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		if v, ok := e.legacy[n]; ok {
			return v, nil
		}
		return zero, fmt.Errorf("the record holds the unknown %s number %d", e.what, n)
	}
	return zero, fmt.Errorf("the record holds no %s", e.what)
}

var (
	states = enum[machine.State]{
		what: "Machine state",
		names: map[machine.State]string{
			machine.StateCreated: "created",
			machine.StateRunning: "running",
			machine.StatePaused:  "paused",
			machine.StateStopped: "stopped",
			machine.StateFaulted: "faulted",
		},
		legacy: map[int]machine.State{
			0: machine.StateCreated,
			1: machine.StateRunning,
			2: machine.StatePaused,
			3: machine.StateStopped,
			4: machine.StateFaulted,
		},
	}
	protocols = enum[machine.Protocol]{
		what: "protocol",
		names: map[machine.Protocol]string{
			machine.ProtocolAny:    "any",
			machine.ProtocolTCP:    "tcp",
			machine.ProtocolUDP:    "udp",
			machine.ProtocolICMP:   "icmp",
			machine.ProtocolICMPv6: "icmpv6",
		},
		legacy: map[int]machine.Protocol{
			0: machine.ProtocolAny,
			1: machine.ProtocolTCP,
			2: machine.ProtocolUDP,
			3: machine.ProtocolICMP,
			4: machine.ProtocolICMPv6,
		},
	}
	execStatuses = enum[machine.ExecStatus]{
		what: "exec status",
		names: map[machine.ExecStatus]string{
			machine.ExecNotStarted: "not-started",
			machine.ExecStarted:    "started",
			machine.ExecExited:     "exited",
			machine.ExecCutShort:   "cut-short",
		},
		legacy: map[int]machine.ExecStatus{
			0: machine.ExecNotStarted,
			1: machine.ExecStarted,
			2: machine.ExecExited,
			3: machine.ExecCutShort,
		},
	}
)

// mapSlice converts a slice, keeping a nil one nil.
func mapSlice[S, T any](in []S, f func(S) (T, error)) ([]T, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]T, len(in))
	for i, v := range in {
		var err error
		if out[i], err = f(v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func infallible[S, T any](f func(S) T) func(S) (T, error) {
	return func(v S) (T, error) { return f(v), nil }
}

func storeSpec(s machine.Spec) (storedSpec, error) {
	disks, _ := mapSlice(s.Disks, infallible(func(a machine.DiskAttachment) storedAttachment {
		return storedAttachment{Disk: a.Disk, ReadOnly: a.ReadOnly, MountPath: a.MountPath}
	}))
	env, _ := mapSlice(s.Env, infallible(func(e machine.EnvVar) storedEnv { return storedEnv{Name: e.Name, Value: e.Value} }))
	files, _ := mapSlice(s.Files, infallible(func(f machine.PlacedFile) storedFile {
		return storedFile{Path: f.Path, Content: f.Content, Mode: f.Mode, UID: f.UID, GID: f.GID}
	}))
	egress, err := mapSlice(s.Policy.Egress, storeRule)
	if err != nil {
		return storedSpec{}, err
	}
	ingress, err := mapSlice(s.Policy.Ingress, storeRule)
	if err != nil {
		return storedSpec{}, err
	}
	return storedSpec{
		Image:         s.Image,
		Resources:     storedResources{VCPUs: s.Resources.VCPUs, MemoryBytes: s.Resources.MemoryBytes},
		Ceiling:       storedResources{VCPUs: s.Ceiling.VCPUs, MemoryBytes: s.Ceiling.MemoryBytes},
		RootDiskBytes: s.RootDiskBytes,
		Disks:         disks,
		Policy:        storedPolicy{Egress: egress, Ingress: ingress},
		Kernel:        s.Kernel,
		Command:       s.Command,
		Args:          s.Args,
		Env:           env,
		User:          s.User,
		Files:         files,
	}, nil
}

func storeRule(r machine.NetworkRule) (storedRule, error) {
	protocol, err := protocols.encode(r.Protocol)
	if err != nil {
		return storedRule{}, err
	}
	ports, _ := mapSlice(r.Ports, infallible(func(p machine.PortRange) storedPorts {
		return storedPorts{First: p.First, Last: p.Last}
	}))
	return storedRule{Prefix: r.Prefix, Protocol: protocol, Ports: ports}, nil
}

func (s storedSpec) load() (machine.Spec, error) {
	disks, _ := mapSlice(s.Disks, infallible(func(a storedAttachment) machine.DiskAttachment {
		return machine.DiskAttachment{Disk: a.Disk, ReadOnly: a.ReadOnly, MountPath: a.MountPath}
	}))
	env, _ := mapSlice(s.Env, infallible(func(e storedEnv) machine.EnvVar { return machine.EnvVar{Name: e.Name, Value: e.Value} }))
	files, _ := mapSlice(s.Files, infallible(func(f storedFile) machine.PlacedFile {
		return machine.PlacedFile{Path: f.Path, Content: f.Content, Mode: f.Mode, UID: f.UID, GID: f.GID}
	}))
	egress, err := mapSlice(s.Policy.Egress, loadRule)
	if err != nil {
		return machine.Spec{}, err
	}
	ingress, err := mapSlice(s.Policy.Ingress, loadRule)
	if err != nil {
		return machine.Spec{}, err
	}
	return machine.Spec{
		Image:         s.Image,
		Resources:     machine.Resources{VCPUs: s.Resources.VCPUs, MemoryBytes: s.Resources.MemoryBytes},
		Ceiling:       machine.Resources{VCPUs: s.Ceiling.VCPUs, MemoryBytes: s.Ceiling.MemoryBytes},
		RootDiskBytes: s.RootDiskBytes,
		Disks:         disks,
		Policy:        machine.NetworkPolicy{Egress: egress, Ingress: ingress},
		Kernel:        s.Kernel,
		Command:       s.Command,
		Args:          s.Args,
		Env:           env,
		User:          s.User,
		Files:         files,
	}, nil
}

func loadRule(r storedRule) (machine.NetworkRule, error) {
	protocol, err := protocols.decode(r.Protocol)
	if err != nil {
		return machine.NetworkRule{}, err
	}
	ports, _ := mapSlice(r.Ports, infallible(func(p storedPorts) machine.PortRange {
		return machine.PortRange{First: p.First, Last: p.Last}
	}))
	return machine.NetworkRule{Prefix: r.Prefix, Protocol: protocol, Ports: ports}, nil
}

func storeMachine(m *machine.Machine) (*storedMachine, error) {
	if m == nil {
		return nil, nil
	}
	state, err := states.encode(m.State)
	if err != nil {
		return nil, err
	}
	spec, err := storeSpec(m.Spec)
	if err != nil {
		return nil, err
	}
	return &storedMachine{
		ID: m.ID, State: state, storedSpec: spec,
		RootDisk: m.RootDisk, RootDiskUUID: m.RootDiskUUID,
		User:          storedUser{UID: m.User.UID, GID: m.User.GID},
		Network:       storedNetwork{MAC: m.Network.MAC, Addresses: m.Network.Addresses, Gateways: m.Network.Gateways, Resolvers: m.Network.Resolvers},
		FailureReason: m.FailureReason, BootMemoryBytes: m.BootMemoryBytes, KernelDigest: m.KernelDigest,
	}, nil
}

func (s *storedMachine) load() (*machine.Machine, error) {
	if s == nil {
		return nil, nil
	}
	state, err := states.decode(s.State)
	if err != nil {
		return nil, err
	}
	spec, err := s.storedSpec.load()
	if err != nil {
		return nil, err
	}
	// A record written before the ceiling and the boot memory existed holds
	// neither: the Machine had no ceiling above its resources, and booted
	// with its memory (ADR 0029).
	spec.Ceiling = spec.Ceiling.OrElse(spec.Resources)
	if s.BootMemoryBytes == 0 {
		s.BootMemoryBytes = spec.Resources.MemoryBytes
	}
	return &machine.Machine{
		BootMemoryBytes: s.BootMemoryBytes,
		ID:              s.ID, State: state, Spec: spec,
		RootDisk: s.RootDisk, RootDiskUUID: s.RootDiskUUID,
		User:          machine.User{UID: s.User.UID, GID: s.User.GID},
		Network:       machine.NetworkIdentity{MAC: s.Network.MAC, Addresses: s.Network.Addresses, Gateways: s.Network.Gateways, Resolvers: s.Network.Resolvers},
		FailureReason: s.FailureReason,
		KernelDigest:  s.KernelDigest,
	}, nil
}

func storeExec(e machine.ExecEntry) (storedExec, error) {
	status, err := execStatuses.encode(e.Record.Status)
	if err != nil {
		return storedExec{}, err
	}
	return storedExec{
		Machine: e.Machine, ID: e.ID, Request: e.Request, Answered: e.Answered,
		Spec:   storedExecSpec{Command: e.Spec.Command, Env: e.Spec.Env, WorkingDir: e.Spec.WorkingDir},
		Record: storedExecRecord{ID: e.Record.ID, Status: status, Reason: e.Record.Reason, ExitCode: e.Record.ExitCode, TermSignal: e.Record.TermSignal, EndedAt: e.Record.EndedAt},
	}, nil
}

func (s storedExec) load() (machine.ExecEntry, error) {
	status, err := execStatuses.decode(s.Record.Status)
	if err != nil {
		return machine.ExecEntry{}, err
	}
	return machine.ExecEntry{
		Machine: s.Machine, ID: s.ID, Request: s.Request, Answered: s.Answered,
		Spec:   machine.ExecSpec{Command: s.Spec.Command, Env: s.Spec.Env, WorkingDir: s.Spec.WorkingDir},
		Record: machine.ExecRecord{ID: s.Record.ID, Status: status, Reason: s.Record.Reason, ExitCode: s.Record.ExitCode, TermSignal: s.Record.TermSignal, EndedAt: s.Record.EndedAt.UTC()},
	}, nil
}
