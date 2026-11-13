package hostnet

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The table mvmd owns is two kernel tables of one name: nftables gives a table
// one family, and ADR 0012 puts a netdev chain at each tap and inet chains
// for the policy. Both are created, replaced and changed in one transaction,
// and neither carries the owner flag, so they outlive the process.
const tableName = "mvmd"

// The inet table's base chains, shared by every Machine.
const (
	chainForward     = "forward"
	chainInput       = "input"
	chainOutput      = "output"
	chainPostrouting = "postrouting"
)

// Ethertypes and protocol numbers the rules name.
const (
	etherIPv4 = unix.ETH_P_IP
	etherARP  = unix.ETH_P_ARP
	etherIPv6 = unix.ETH_P_IPV6

	icmpv6NeighbourSolicit = 135
	icmpv6NeighbourAdvert  = 136
	icmpv6MLDv1First       = 130 // listener query, up to the redirect at 137
	icmpv6Redirect         = 137
	icmpv6MLDv2Report      = 143
)

type tables struct {
	netdev, inet *nftables.Table
}

func newTables() tables {
	return tables{
		netdev: &nftables.Table{Family: nftables.TableFamilyNetdev, Name: tableName},
		inet:   &nftables.Table{Family: nftables.TableFamilyINet, Name: tableName},
	}
}

func (t tables) inetChain(name string) *nftables.Chain {
	return &nftables.Chain{Table: t.inet, Name: name}
}

// egressChain and ingressChain are the Machine's policy chains in the inet
// table: what its packets may send, and what may be sent to it.
func egressChain(tap string) string  { return "eg_" + tap }
func ingressChain(tap string) string { return "in_" + tap }

// addBase adds the tables, the base chains and the rules that do not belong to
// one Machine: the VMM users' output drop (ADR 0015) and the masquerade of
// what leaves the node (ADR 0011). Adding a table that exists changes nothing.
func (n *Network) addBase(c *nftables.Conn, t tables) {
	c.AddTable(t.netdev)
	c.AddTable(t.inet)
	filter := func(name string, hook *nftables.ChainHook) *nftables.Chain {
		return c.AddChain(&nftables.Chain{
			Table: t.inet, Name: name, Type: nftables.ChainTypeFilter,
			Hooknum: hook, Priority: nftables.ChainPriorityFilter, Policy: &policyAccept,
		})
	}
	filter(chainForward, nftables.ChainHookForward)
	filter(chainInput, nftables.ChainHookInput)
	output := filter(chainOutput, nftables.ChainHookOutput)
	post := c.AddChain(&nftables.Chain{
		Table: t.inet, Name: chainPostrouting, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource,
	})

	r := n.cfg.VMMUIDRange
	c.AddRule(&nftables.Rule{
		Table: t.inet, Chain: output,
		Exprs: join(skuidIn(r.First, r.First+r.Count-1), verdict(expr.VerdictDrop)),
	})
	// An accept ends the chain, so a destination in a no-masquerade prefix
	// skips the masquerade rules below it.
	for _, p := range n.cfg.NoMasquerade {
		c.AddRule(&nftables.Rule{
			Table: t.inet, Chain: post,
			Exprs: join(nfprotoIs(familyOf(p.Addr())), addrIn(p, true), verdict(expr.VerdictAccept)),
		})
	}
	// What goes out of another Machine's tap is routed, not translated: that is
	// a routed prefix of that Machine (ADR 0061), whose source the guest must see.
	for _, p := range []netip.Prefix{n.cfg.IPv4Prefix, n.cfg.IPv6Prefix} {
		if !p.IsValid() {
			continue
		}
		c.AddRule(&nftables.Rule{
			Table: t.inet, Chain: post,
			Exprs: join(nfprotoIs(familyOf(p.Addr())), addrIn(p, false), addrOutside(p, true), oifnameNotStarting(tapPrefix), []expr.Any{&expr.Masq{}}),
		})
	}
}

// machinePlan is everything one Machine adds to the kernel: its chains, then
// the rules in them and the jumps to them from the base chains.
type machinePlan struct {
	chains []*nftables.Chain
	rules  []*nftables.Rule
}

func (p *machinePlan) rule(t *nftables.Table, chain *nftables.Chain, tag string, exprs []expr.Any) {
	r := &nftables.Rule{Table: t, Chain: chain, Exprs: exprs}
	if tag != "" {
		r.UserData = []byte(tag)
	}
	p.rules = append(p.rules, r)
}

// plan makes the chains and rules of the Machine. It touches no kernel, so a
// policy it cannot express is refused before anything changes.
func (t tables) plan(m machineNet) (*machinePlan, error) {
	p := &machinePlan{}

	tapChain := &nftables.Chain{
		Table: t.netdev, Name: m.tap, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookIngress, Priority: nftables.ChainPriorityFilter,
		Device: m.tap, Policy: &policyDrop,
	}
	eg := &nftables.Chain{Table: t.inet, Name: egressChain(m.tap)}
	in := &nftables.Chain{Table: t.inet, Name: ingressChain(m.tap)}
	p.chains = []*nftables.Chain{tapChain, eg, in}

	p.tapRules(t, tapChain, m)

	for _, side := range []struct {
		chain  *nftables.Chain
		rules  []machine.NetworkRule
		egress bool
	}{{eg, m.policy.Egress, true}, {in, m.policy.Ingress, false}} {
		matched := verdict(expr.VerdictAccept)
		if side.egress {
			// A packet the policy allows leaves the chain and goes on to
			// the receiver's ingress check.
			matched = verdict(expr.VerdictReturn)
		}
		p.rule(t.inet, side.chain, "", join(ctStateIs(expr.CtStateBitINVALID), verdict(expr.VerdictDrop)))
		p.rule(t.inet, side.chain, "", join(ctStateIs(expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED), matched))
		for _, r := range side.rules {
			matches, err := ruleMatches(r, side.egress)
			if err != nil {
				return nil, err
			}
			for _, match := range matches {
				p.rule(t.inet, side.chain, "", join(match, matched))
			}
		}
		p.rule(t.inet, side.chain, "", verdict(expr.VerdictDrop))
	}

	p.baseRules(t, m)
	return p, nil
}

// tapRules is the check at the tap (ADR 0012): the Machine's frames, by family
// the Machine holds, and nothing else. The chain's policy drops the rest.
func (p *machinePlan) tapRules(t tables, chain *nftables.Chain, m machineNet) {
	add := func(parts ...[]expr.Any) {
		p.rule(t.netdev, chain, "", join(etherSaddrIs(m.mac), join(parts...)))
	}
	accept := verdict(expr.VerdictAccept)
	if m.ip4.IsValid() {
		arp := func(parts ...[]expr.Any) []expr.Any {
			return join(etherTypeIs(etherARP),
				arpField(0, be16(1)),          // Ethernet
				arpField(2, be16(etherIPv4)),  // of IPv4 addresses
				arpField(4, []byte{6}),        // hardware address length
				arpField(5, []byte{4}),        // protocol address length
				arpField(8, m.mac),            // sender hardware address
				arpField(14, m.ip4.AsSlice()), // sender protocol address
				join(parts...))
		}
		add(arp(arpField(6, be16(1)), arpField(24, m.gw4.AsSlice())), accept) // a request for the gateway
		add(arp(arpField(6, be16(2))), accept)                                // a reply
		add(etherTypeIs(etherIPv4), addrIs(m.ip4, false), accept)
	}
	// A routed prefix is the Machine's own as a source, from this tap only
	// (ADR 0061). A frame of a family the Machine holds no address in cannot
	// carry one, because the node serves only the families it gave addresses.
	for _, rp := range m.routed[:0] {
		if rp.Addr().Is4() {
			add(etherTypeIs(etherIPv4), addrIn(rp, false), accept)
		}
	}
	if m.ip6.IsValid() {
		v6 := etherTypeIs(etherIPv6)
		icmpv6 := l4protoIs(unix.IPPROTO_ICMPV6)
		sources := []netip.Addr{m.ip6, m.ll6}
		for _, src := range sources {
			add(v6, addrIs(src, false), icmpv6, icmpv6TypeIn(icmpv6NeighbourSolicit, icmpv6NeighbourAdvert), ip6Hoplimit255(), accept)
		}
		// Router advertisements, redirects and router solicitations, MLD,
		// and neighbour discovery that did not pass above.
		add(v6, icmpv6, icmpv6TypeIn(icmpv6MLDv1First, icmpv6Redirect), verdict(expr.VerdictDrop))
		add(v6, icmpv6, icmpv6TypeIn(icmpv6MLDv2Report, icmpv6MLDv2Report), verdict(expr.VerdictDrop))
		for _, src := range sources {
			add(v6, addrIs(src, false), accept)
		}
		for _, rp := range m.routed[:0] {
			if rp.Addr().Is6() {
				add(v6, addrIn(rp, false), accept)
			}
		}
	}
}

// arpField matches a field of the ARP header, which starts the network header.
func arpField(offset uint32, data []byte) []expr.Any {
	return []expr.Any{payload(expr.PayloadBaseNetworkHeader, offset, uint32(len(data))), cmp(expr.CmpOpEq, data)}
}

// baseRules are the jumps from the base chains to the Machine's chains, each
// tagged with the tap so that removal finds them.
func (p *machinePlan) baseRules(t tables, m machineNet) {
	tag := m.tap
	eg, in := egressChain(m.tap), ingressChain(m.tap)
	from, to := iifnameIs(m.tap), oifnameIs(m.tap)
	accept := verdict(expr.VerdictAccept)

	p.rule(t.inet, t.inetChain(chainForward), tag, join(from, jump(eg)))
	p.rule(t.inet, t.inetChain(chainForward), tag, join(to, jump(in)))

	// Neighbour discovery is untracked, so no connection state admits it. It
	// passes between the host and the Machine, and nothing else untracked does.
	nd := func(dir []expr.Any) []expr.Any {
		return join(dir, nfprotoIs(unix.NFPROTO_IPV6), l4protoIs(unix.IPPROTO_ICMPV6),
			icmpv6TypeIn(icmpv6NeighbourSolicit, icmpv6NeighbourAdvert), ip6Hoplimit255(), accept)
	}
	if m.ip6.IsValid() {
		p.rule(t.inet, t.inetChain(chainInput), tag, nd(from))
	}
	p.rule(t.inet, t.inetChain(chainInput), tag, join(from, jump(eg)))
	p.rule(t.inet, t.inetChain(chainInput), tag, join(from, accept))

	if m.ip6.IsValid() {
		p.rule(t.inet, t.inetChain(chainOutput), tag, nd(to))
	}
	p.rule(t.inet, t.inetChain(chainOutput), tag, join(to, jump(in)))
	p.rule(t.inet, t.inetChain(chainOutput), tag, join(to, accept))

	// What leaves the node from a routed prefix is masqueraded as what leaves
	// from the Machine's address is (ADR 0049, ADR 0061), but not what goes to
	// another Machine's tap, which the node routes and does not translate. The
	// rules follow the base chain's no-masquerade accepts.
	for _, rp := range m.routed {
		node := m.node4
		if rp.Addr().Is6() {
			node = m.node6
		}
		p.rule(t.inet, t.inetChain(chainPostrouting), tag,
			join(nfprotoIs(familyOf(rp.Addr())), addrIn(rp, false), addrOutside(node, true), oifnameNotStarting(tapPrefix), []expr.Any{&expr.Masq{}}))
	}
}

// add queues the plan in the order the kernel needs: a chain before its rules.
func (p *machinePlan) add(c *nftables.Conn) {
	for _, ch := range p.chains {
		c.AddChain(ch)
	}
	for _, r := range p.rules {
		c.AddRule(r)
	}
}

// ruleMatches are the matches of one policy rule, as one per port range, or
// one for a rule with no ports. An egress rule matches the destination, an
// ingress rule the source.
func ruleMatches(r machine.NetworkRule, egress bool) ([][]expr.Any, error) {
	if !r.Prefix.IsValid() {
		return nil, fmt.Errorf("a network rule has no valid prefix")
	}
	v4 := r.Prefix.Addr().Is4()
	var proto []expr.Any
	switch r.Protocol {
	case machine.ProtocolAny:
	case machine.ProtocolTCP:
		proto = l4protoIs(unix.IPPROTO_TCP)
	case machine.ProtocolUDP:
		proto = l4protoIs(unix.IPPROTO_UDP)
	case machine.ProtocolICMP:
		if !v4 {
			return nil, fmt.Errorf("ICMP rule %s needs an IPv4 prefix", r.Prefix)
		}
		proto = l4protoIs(unix.IPPROTO_ICMP)
	case machine.ProtocolICMPv6:
		if v4 {
			return nil, fmt.Errorf("ICMPv6 rule %s needs an IPv6 prefix", r.Prefix)
		}
		proto = l4protoIs(unix.IPPROTO_ICMPV6)
	default:
		return nil, fmt.Errorf("rule %s has an unknown protocol", r.Prefix)
	}
	if len(r.Ports) > 0 && r.Protocol != machine.ProtocolTCP && r.Protocol != machine.ProtocolUDP {
		return nil, fmt.Errorf("rule %s sets ports on a protocol that has none", r.Prefix)
	}
	where := join(nfprotoIs(familyOf(r.Prefix.Addr())), addrIn(r.Prefix, egress), proto)
	if len(r.Ports) == 0 {
		return [][]expr.Any{where}, nil
	}
	var out [][]expr.Any
	for _, pr := range r.Ports {
		if pr.First < 1 || pr.First > pr.Last {
			return nil, fmt.Errorf("rule %s has the port range %d-%d", r.Prefix, pr.First, pr.Last)
		}
		out = append(out, join(where, portIn(pr.First, pr.Last)))
	}
	return out, nil
}
