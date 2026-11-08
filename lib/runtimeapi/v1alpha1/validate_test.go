package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestImageRef_Validate(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		digest     string
		valid      bool
	}{
		{"plain repository", "registry.example/ns/app", digest, true},
		{"host with a port is not a tag", "registry:5000/app", digest, true},
		{"tag on the last segment", "app:latest", digest, false},
		{"tag after a host with a port", "registry:5000/app:1.2", digest, false},
		{"digest inside the repository", "app@" + digest, digest, false},
		{"empty repository", "", digest, false},
		{"tag in place of the digest", "registry.example/app", "latest", false},
		{"uppercase digest", "registry.example/app", "sha256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&v1alpha1.ImageRef{Repository: tc.repository, Digest: tc.digest}).Validate()
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestCreateMachineRequest_Validate_RefusesNonDigestImage(t *testing.T) {
	ok := &v1alpha1.CreateMachineRequest{MachineId: "m", RequestId: "r", ImageDigest: digest}
	assert.NoError(t, ok.Validate())

	tagged := &v1alpha1.CreateMachineRequest{MachineId: "m", RequestId: "r", ImageDigest: "app:latest"}
	assert.Error(t, tagged.Validate())
}

func TestNetworkRule_Validate(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		protocol v1alpha1.Protocol
		ports    []*v1alpha1.PortRange
		valid    bool
	}{
		// Valid: IPv4 TCP with a port range.
		{"ipv4 tcp port range", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_TCP, []*v1alpha1.PortRange{{First: 80, Last: 443}}, true},
		// Valid: IPv4 TCP, no ports (every port).
		{"ipv4 tcp no ports", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_TCP, nil, true},
		// Valid: single-port range.
		{"ipv4 tcp single port", "192.168.1.0/24", v1alpha1.Protocol_PROTOCOL_TCP, []*v1alpha1.PortRange{{First: 80, Last: 80}}, true},
		// Valid: IPv6 UDP.
		{"ipv6 udp", "fd00::/48", v1alpha1.Protocol_PROTOCOL_UDP, []*v1alpha1.PortRange{{First: 53, Last: 53}}, true},
		// Valid: ICMPv6 with IPv6 prefix.
		{"icmpv6 ipv6", "fd00::/64", v1alpha1.Protocol_PROTOCOL_ICMPV6, nil, true},
		// Valid: ICMP with IPv4 prefix.
		{"icmp ipv4", "10.0.0.0/8", v1alpha1.Protocol_PROTOCOL_ICMP, nil, true},
		// Valid: any protocol with IPv4.
		{"any ipv4", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_ANY, nil, true},
		// Invalid: unspecified protocol.
		{"unspecified protocol", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_UNSPECIFIED, nil, false},
		// Invalid: out-of-range protocol value.
		{"out-of-range protocol", "10.0.0.0/24", v1alpha1.Protocol(99), nil, false},
		// Valid: single address /32.
		{"ipv4 /32", "192.168.0.1/32", v1alpha1.Protocol_PROTOCOL_TCP, []*v1alpha1.PortRange{{First: 22, Last: 22}}, true},
		// Valid: single address /128.
		{"ipv6 /128", "::1/128", v1alpha1.Protocol_PROTOCOL_TCP, nil, true},

		// Invalid: unparseable prefix.
		{"bad prefix", "not-a-cidr", v1alpha1.Protocol_PROTOCOL_TCP, nil, false},
		// Invalid: empty prefix.
		{"empty prefix", "", v1alpha1.Protocol_PROTOCOL_TCP, nil, false},
		// Invalid: prefix without mask.
		{"prefix no mask", "10.0.0.0", v1alpha1.Protocol_PROTOCOL_TCP, nil, false},
		// Invalid: ICMP with IPv6 prefix.
		{"icmp ipv6 prefix", "fd00::/64", v1alpha1.Protocol_PROTOCOL_ICMP, nil, false},
		// Invalid: ICMPv6 with IPv4 prefix (near miss: ICMPv6 with IPv6 is valid).
		{"icmpv6 ipv4 prefix", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_ICMPV6, nil, false},
		// Invalid: port outside range (first below 1).
		{"port first zero", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_TCP, []*v1alpha1.PortRange{{First: 0, Last: 80}}, false},
		// Invalid: port outside range (above 65535).
		{"port above 65535", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_TCP, []*v1alpha1.PortRange{{First: 80, Last: 70000}}, false},
		// Invalid: first above last (near miss: valid with first == last).
		{"first above last", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_TCP, []*v1alpha1.PortRange{{First: 443, Last: 80}}, false},
		// Invalid: ports on ICMP rule.
		{"icmp with ports", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_ICMP, []*v1alpha1.PortRange{{First: 80, Last: 80}}, false},
		// Invalid: ports on ICMPv6 rule (near miss: ICMPv6 without ports is valid).
		{"icmpv6 with ports", "fd00::/64", v1alpha1.Protocol_PROTOCOL_ICMPV6, []*v1alpha1.PortRange{{First: 80, Last: 80}}, false},
		// Invalid: ports on any protocol rule.
		{"any with ports", "10.0.0.0/24", v1alpha1.Protocol_PROTOCOL_ANY, []*v1alpha1.PortRange{{First: 80, Last: 80}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&v1alpha1.NetworkRule{
				Prefix:   tc.prefix,
				Protocol: tc.protocol,
				Ports:    tc.ports,
			}).Validate()
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestNetworkPolicy_Validate(t *testing.T) {
	ipv4Rule := &v1alpha1.NetworkRule{Prefix: "10.0.0.0/24", Protocol: v1alpha1.Protocol_PROTOCOL_TCP, Ports: []*v1alpha1.PortRange{{First: 80, Last: 443}}}
	badRule := &v1alpha1.NetworkRule{Prefix: "fd00::/64", Protocol: v1alpha1.Protocol_PROTOCOL_ICMP}

	// Valid: empty policy.
	assert.NoError(t, (&v1alpha1.NetworkPolicy{}).Validate())

	// Valid: one good rule per direction.
	assert.NoError(t, (&v1alpha1.NetworkPolicy{
		Egress:  []*v1alpha1.NetworkRule{ipv4Rule},
		Ingress: []*v1alpha1.NetworkRule{ipv4Rule},
	}).Validate())

	// Valid: multiple good rules.
	assert.NoError(t, (&v1alpha1.NetworkPolicy{
		Egress: []*v1alpha1.NetworkRule{ipv4Rule, ipv4Rule},
	}).Validate())

	// Invalid: one bad rule in egress.
	assert.Error(t, (&v1alpha1.NetworkPolicy{
		Egress: []*v1alpha1.NetworkRule{badRule},
	}).Validate())

	// Invalid: one bad rule in ingress.
	assert.Error(t, (&v1alpha1.NetworkPolicy{
		Ingress: []*v1alpha1.NetworkRule{badRule},
	}).Validate())

	// Invalid: mix of good and bad.
	assert.Error(t, (&v1alpha1.NetworkPolicy{
		Egress:  []*v1alpha1.NetworkRule{ipv4Rule, badRule},
		Ingress: []*v1alpha1.NetworkRule{ipv4Rule},
	}).Validate())
}

func TestCreateMachineRequest_Validate_RefusesInvalidPolicy(t *testing.T) {
	ok := &v1alpha1.CreateMachineRequest{
		MachineId:   "m",
		RequestId:   "r",
		ImageDigest: digest,
		NetworkPolicy: &v1alpha1.NetworkPolicy{
			Egress: []*v1alpha1.NetworkRule{{Prefix: "10.0.0.0/24", Protocol: v1alpha1.Protocol_PROTOCOL_TCP, Ports: []*v1alpha1.PortRange{{First: 80, Last: 80}}}},
		},
	}
	assert.NoError(t, ok.Validate())

	invalid := &v1alpha1.CreateMachineRequest{
		MachineId:   "m",
		RequestId:   "r",
		ImageDigest: digest,
		NetworkPolicy: &v1alpha1.NetworkPolicy{
			Egress: []*v1alpha1.NetworkRule{{Prefix: "fd00::/64", Protocol: v1alpha1.Protocol_PROTOCOL_ICMP}},
		},
	}
	assert.Error(t, invalid.Validate())
}

func TestRestoreMachineRequest_Validate_RefusesInvalidPolicy(t *testing.T) {
	ok := &v1alpha1.RestoreMachineRequest{
		MachineId:  "m",
		RequestId:  "r",
		SnapshotId: "snap",
		NetworkPolicy: &v1alpha1.NetworkPolicy{
			Ingress: []*v1alpha1.NetworkRule{{Prefix: "10.0.0.0/24", Protocol: v1alpha1.Protocol_PROTOCOL_TCP, Ports: []*v1alpha1.PortRange{{First: 80, Last: 80}}}},
		},
	}
	assert.NoError(t, ok.Validate())

	invalid := &v1alpha1.RestoreMachineRequest{
		MachineId:  "m",
		RequestId:  "r",
		SnapshotId: "snap",
		NetworkPolicy: &v1alpha1.NetworkPolicy{
			Ingress: []*v1alpha1.NetworkRule{{Prefix: "10.0.0.0/24", Protocol: v1alpha1.Protocol_PROTOCOL_UNSPECIFIED}},
		},
	}
	assert.Error(t, invalid.Validate())
}
