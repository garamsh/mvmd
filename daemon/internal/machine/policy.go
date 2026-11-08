package machine

import (
	"fmt"
	"net/netip"
	"slices"
)

// Protocol is the protocol a network rule matches.
type Protocol int

const (
	// ProtocolAny matches any protocol of the prefix's family.
	ProtocolAny Protocol = iota
	ProtocolTCP
	ProtocolUDP
	// ProtocolICMP is IPv4 only.
	ProtocolICMP
	// ProtocolICMPv6 is IPv6 only.
	ProtocolICMPv6
)

// PortRange is an inclusive range of ports.
type PortRange struct {
	First, Last uint16
}

// NetworkRule allows packets to (egress) or from (ingress) a prefix.
type NetworkRule struct {
	Prefix   netip.Prefix
	Protocol Protocol
	// Ports is empty for every port; only TCP and UDP rules may set it.
	Ports []PortRange
}

// NetworkPolicy is an allow-list. Both lists empty blocks all traffic.
type NetworkPolicy struct {
	Egress  []NetworkRule
	Ingress []NetworkRule
}

func (p NetworkPolicy) clone() NetworkPolicy {
	p.Egress = cloneRules(p.Egress)
	p.Ingress = cloneRules(p.Ingress)
	return p
}

func cloneRules(rules []NetworkRule) []NetworkRule {
	rules = slices.Clone(rules)
	for i := range rules {
		rules[i].Ports = slices.Clone(rules[i].Ports)
	}
	return rules
}

// validate rejects a policy by the rules of runtime-api.md §Network policy.
func (p NetworkPolicy) validate() error {
	for _, rules := range [][]NetworkRule{p.Egress, p.Ingress} {
		for _, r := range rules {
			if err := r.validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r NetworkRule) validate() error {
	if !r.Prefix.IsValid() {
		return fmt.Errorf("network rule has an unparseable prefix")
	}
	v4 := r.Prefix.Addr().Is4()
	switch {
	case r.Protocol == ProtocolICMP && !v4:
		return fmt.Errorf("ICMP rule %s needs an IPv4 prefix", r.Prefix)
	case r.Protocol == ProtocolICMPv6 && v4:
		return fmt.Errorf("ICMPv6 rule %s needs an IPv6 prefix", r.Prefix)
	case len(r.Ports) > 0 && r.Protocol != ProtocolTCP && r.Protocol != ProtocolUDP:
		return fmt.Errorf("rule %s sets ports on a protocol that has none", r.Prefix)
	}
	for _, pr := range r.Ports {
		if pr.First < 1 || pr.First > pr.Last {
			return fmt.Errorf("rule %s has the port range %d-%d", r.Prefix, pr.First, pr.Last)
		}
	}
	return nil
}
