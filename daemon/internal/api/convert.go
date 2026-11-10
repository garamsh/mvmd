package api

import (
	"fmt"
	"net/netip"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
	"google.golang.org/protobuf/types/known/durationpb"
)

var protocols = map[v1alpha2.Protocol]machine.Protocol{
	v1alpha2.Protocol_PROTOCOL_TCP:    machine.ProtocolTCP,
	v1alpha2.Protocol_PROTOCOL_UDP:    machine.ProtocolUDP,
	v1alpha2.Protocol_PROTOCOL_ICMP:   machine.ProtocolICMP,
	v1alpha2.Protocol_PROTOCOL_ICMPV6: machine.ProtocolICMPv6,
	v1alpha2.Protocol_PROTOCOL_ANY:    machine.ProtocolAny,
}

var states = map[machine.State]v1alpha2.MachineState{
	machine.StateCreated: v1alpha2.MachineState_MACHINE_STATE_CREATED,
	machine.StateRunning: v1alpha2.MachineState_MACHINE_STATE_RUNNING,
	machine.StatePaused:  v1alpha2.MachineState_MACHINE_STATE_PAUSED,
	machine.StateStopped: v1alpha2.MachineState_MACHINE_STATE_STOPPED,
	machine.StateFaulted: v1alpha2.MachineState_MACHINE_STATE_FAULTED,
}

var phases = map[fencing.Phase]v1alpha2.DeadlinePhase{
	fencing.PhaseUnarmed: v1alpha2.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED,
	fencing.PhaseArmed:   v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED,
	fencing.PhaseExpired: v1alpha2.DeadlinePhase_DEADLINE_PHASE_EXPIRED,
	fencing.PhaseStopped: v1alpha2.DeadlinePhase_DEADLINE_PHASE_STOPPED_EVERYTHING,
}

func resourcesOf(r *v1alpha2.Resources) machine.Resources {
	return machine.Resources{VCPUs: r.GetCpuCount(), MemoryBytes: r.GetMemoryBytes()}
}

func resourcesToWire(r machine.Resources) *v1alpha2.Resources {
	return &v1alpha2.Resources{CpuCount: r.VCPUs, MemoryBytes: r.MemoryBytes}
}

func rulesOf(in []*v1alpha2.NetworkRule) ([]machine.NetworkRule, error) {
	var out []machine.NetworkRule
	for _, r := range in {
		prefix, err := netip.ParsePrefix(r.GetPrefix())
		if err != nil {
			return nil, fmt.Errorf("network rule prefix %q: %w", r.GetPrefix(), err)
		}
		rule := machine.NetworkRule{Prefix: prefix, Protocol: protocols[r.GetProtocol()]}
		for _, p := range r.GetPorts() {
			if p.GetFirst() > 65535 || p.GetLast() > 65535 {
				return nil, fmt.Errorf("port range %d-%d is out of range", p.GetFirst(), p.GetLast())
			}
			rule.Ports = append(rule.Ports, machine.PortRange{First: uint16(p.GetFirst()), Last: uint16(p.GetLast())})
		}
		out = append(out, rule)
	}
	return out, nil
}

func specOf(req *v1alpha2.CreateMachineRequest) (machine.Spec, error) {
	spec := machine.Spec{
		Image:         machine.ImageDigest(req.GetImageDigest()),
		Resources:     resourcesOf(req.GetResources()),
		Ceiling:       resourcesOf(req.GetResourceCeiling()),
		RootDiskBytes: req.GetRootDiskBytes(),
	}
	for _, d := range req.GetDisks() {
		spec.Disks = append(spec.Disks, machine.DiskAttachment{Disk: machine.DiskID(d.GetDiskId()), ReadOnly: d.GetReadOnly()})
	}
	var err error
	if spec.Policy.Egress, err = rulesOf(req.GetNetworkPolicy().GetEgress()); err != nil {
		return spec, err
	}
	spec.Policy.Ingress, err = rulesOf(req.GetNetworkPolicy().GetIngress())
	return spec, err
}

func rulesToWire(in []machine.NetworkRule) []*v1alpha2.NetworkRule {
	var out []*v1alpha2.NetworkRule
	for _, r := range in {
		rule := &v1alpha2.NetworkRule{Prefix: r.Prefix.String()}
		for wire, p := range protocols {
			if p == r.Protocol {
				rule.Protocol = wire
			}
		}
		for _, p := range r.Ports {
			rule.Ports = append(rule.Ports, &v1alpha2.PortRange{First: uint32(p.First), Last: uint32(p.Last)})
		}
		out = append(out, rule)
	}
	return out
}

func machineToWire(m machine.Machine) *v1alpha2.Machine {
	out := &v1alpha2.Machine{
		Id:                string(m.ID),
		State:             states[m.State],
		LastFailureReason: m.FailureReason,
		Driver:            &v1alpha2.Driver{Name: m.Driver.Name, Version: m.Driver.Version},
		Resources:         resourcesToWire(m.Resources),
		ResourceCeiling:   resourcesToWire(m.Ceiling),
		RootDiskBytes:     m.RootDiskBytes,
		NetworkPolicy: &v1alpha2.NetworkPolicy{
			Egress:  rulesToWire(m.Policy.Egress),
			Ingress: rulesToWire(m.Policy.Ingress),
		},
	}
	for _, d := range m.Disks {
		out.Disks = append(out.Disks, &v1alpha2.DiskAttachment{DiskId: string(d.Disk), ReadOnly: d.ReadOnly})
	}
	for _, a := range m.Network.Addresses {
		out.Addresses = append(out.Addresses, a.String())
	}
	return out
}

func deadlineToWire(st fencing.Status) *v1alpha2.DeadlineStatus {
	return &v1alpha2.DeadlineStatus{
		Phase:             phases[st.Phase],
		Length:            durationpb.New(st.Length),
		Remaining:         durationpb.New(st.Remaining),
		LastRenewalNumber: st.Renewal,
	}
}
