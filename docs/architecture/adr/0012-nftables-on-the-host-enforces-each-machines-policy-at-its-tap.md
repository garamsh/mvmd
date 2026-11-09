# ADR 0012: nftables on the host enforces each Machine's policy at its tap

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#17 and decided in review of the pull request that adds this ADR.

`runtime.md` §Open questions left the enforcement technology open, naming nftables and eBPF from garamsh/discussions#2 §6 item 10 (discussioncomment-19288322). The rules it must keep: a Machine does not start until its policy is applied, the default blocks all external traffic (#2 §4, discussioncomment-19288320), and the policy is enforced outside the guest (#2 §3, discussioncomment-19288319). Each Machine's traffic crosses one routed tap ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)).

From nftables 1.1.7's documentation (`doc/nft.txt`, `doc/statements.txt` and `doc/data-types.txt`, as packaged in Debian `nftables` 1.1.7-3):

- The `netdev` family "handles packets from the device ingress and egress path" and filters "packets of any ethertype such as ARP … as well as IPv4 and IPv6 packets". Its ingress hook is invoked "right after *tc* ingress and before layer 3 protocol handlers". Its base chains "require the 'DEVICE' parameter … since they exist per interface only".
- The `inet` family has the `prerouting`, `input`, `forward`, `output` and `postrouting` hooks. `ct state established,related` matches connection-tracking state, and `masquerade` "makes sense only in postrouting".
- A table with the `owner` flag "will be removed when the process exits" unless it also carries `persist`.
- `icmpv6 type` takes the keywords `nd-router-solicit` (133), `nd-router-advert` (134), `nd-neighbor-solicit` (135), `nd-neighbor-advert` (136), `nd-redirect` (137), `mld-listener-query`, `-report` and `-done` (130 to 132), and the error types `destination-unreachable`, `packet-too-big`, `time-exceeded` and `parameter-problem` (1 to 4).
- NAT statements work in the inet family "(available with kernel 5.2)", and the example `add rule inet nat postrouting meta oif ppp0 masquerade` is annotated "this masquerades both ipv4 and ipv6" (`doc/statements.txt`).

From Linux v6.12:

- `net/netfilter/nfnetlink.c`, `nfnetlink_rcv_batch`: the messages of one batch are committed (`ss->commit`) only when the whole batch was processed without failure; otherwise the subsystem aborts (`ss->abort`) and none of the batch takes effect.
- `net/netfilter/Kconfig`: `NETFILTER_INGRESS` and `NETFILTER_EGRESS` default to `y`.
- **Neighbour discovery is untracked.** `nf_conntrack_icmpv6_error` (`net/netfilter/nf_conntrack_proto_icmpv6.c`) sets MLD queries, reports and reductions, router solicitations and advertisements, and neighbour solicitations and advertisements to `IP_CT_UNTRACKED`, through the `noct_valid_new` table. So `ct state established,related` never matches them, and they pass only where a rule names them.
- **ICMPv6 errors are related.** The same function passes error types (below 128) to `nf_conntrack_inet_error` (`net/netfilter/nf_conntrack_proto_icmp.c`). That function looks up the connection the embedded packet belongs to and sets `IP_CT_RELATED`, so `packet-too-big` for an accepted connection matches `related`. Redirects are handled apart (`nf_conntrack_icmpv6_redirect`).
- `ndisc_rcv` drops a neighbour discovery message whose hop limit is not 255 (`net/ipv6/ndisc.c`).

From iproute2 v6.12.0, `man/man8/tc-bpf.8`: eBPF classifiers and actions can be "attached to ingress and egress qdiscs", in `direct-action` mode return the verdict themselves (`TC_ACT_OK`, `TC_ACT_SHOT`), and are checked by a verifier that "guarantees that a specified program always terminates".

Candidates weighed:

- **nftables, one table owned by mvmd, with a netdev chain per tap and inet chains for the policy** — chosen.
- **eBPF programs on each tap through tc.** As well placed as a netdev chain, and more programmable. mvmd would own a BPF program and its loader, keep it working across the kernels nodes run, and build in C what nftables already has: connection tracking for replies, address and port sets, NAT, and an all-or-nothing update.
- **iptables and ebtables.** The ARP and anti-spoofing checks and the IP policy would be split between tools, with no single transaction covering both.
- **A firewall in the guest.** The guest is untrusted (#2 §5, security).
- **A VMM's own rate limiter.** It shapes traffic and filters nothing.
- **A container network plugin (CNI).** Excluded by ADR 0001.

## Decision

mvmd enforces every Machine's network policy with nftables in the host kernel, in one table it owns, changed only by transactions. A Machine's rules are committed before its tap's link comes up, so no packet crosses the tap before its policy.

- **At the tap (netdev ingress).** Every frame the guest sends must carry the Machine's MAC. By family, for each family the node serves; a family it does not serve is dropped entirely:
  - **IPv4.** ARP is accepted only as a request for the IPv4 gateway or a reply, with the Machine's MAC and IPv4 address as sender. IPv4 packets are accepted only from the Machine's IPv4 address.
  - **IPv6.** Packets are accepted only from the Machine's IPv6 address or from its EUI-64 link-local address ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)).
  - **ICMPv6 neighbour discovery.** `nd-neighbor-solicit` and `nd-neighbor-advert` are accepted with hop limit 255 from those sources, which is all the guest needs to resolve `fe80::1` and to be resolved.
  - **Dropped ICMPv6.** `nd-router-advert` and `nd-redirect` are dropped, so no guest can announce itself as a router or redirect another's traffic. `nd-router-solicit` is dropped too: no router advertisement service exists to answer it. MLD messages are dropped, since a point-to-point tap has no multicast group to manage.
  - **Everything else** is dropped: other ethertypes, other sources, and DAD probes from the unspecified address, which the agent turns off. No guest can send as another.
- **Neighbour discovery between host and guest (inet).** Because conntrack leaves it untracked, the input chain accepts `nd-neighbor-solicit` and `nd-neighbor-advert` from a Machine's tap, and the output chain accepts them to it, with hop limit 255. No other untracked ICMPv6 is accepted in either direction. ICMPv6 is otherwise subject to the policy, and its errors pass as `related` to the connection they report on.
- **The policy (inet).** Forward and input chains accept a packet from a Machine only if an egress rule of its policy matches, and to a Machine only if an ingress rule matches ([ADR 0013](./0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md)). Replies of an accepted connection are accepted by connection-tracking state. Anything else from or to a Machine's tap is dropped, the host's own services included. A packet between two Machines on the node needs the sender's egress and the receiver's ingress.
- **Leaving the node.** One inet postrouting rule masquerades accepted traffic, of either family, whose destination is outside the node's prefixes (ADR 0011).
- **Order at Start and Restore.** Create the tap with its link down; commit the Machine's chains in one transaction; bring the link up; start the VMM. A transaction that fails changes nothing, the tap is deleted, and the call fails in its prior state, so the Machine never runs unfiltered.
- **Order at the end.** After the VMM process has ended, one transaction removes the Machine's chains, and the tap is deleted.
- **Across restarts of mvmd.** The table is created without the `owner` flag, so it stays in the kernel while mvmd is down and the running Machines stay filtered. Before serving any call, mvmd replaces the table in one transaction with the one its durable state describes.
- **Guarantee and its limit.** No path from a guest to the network bypasses the tap's chains. A privileged host process that deletes or alters the table is outside the guarantee, as it is for the fencing supervisor ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)).

## Consequences

- The enforcement is the kernel's: mvmd writes declarative rules, and no packet is handled by a mvmd process.
- Applying a policy costs one transaction per Start or Restore, so start latency includes it.
- The host needs nftables in its kernel (`CONFIG_NF_TABLES`, the netdev and inet families, connection tracking and NAT) and mvmd needs `CAP_NET_ADMIN`.
- Another tool that flushes the whole ruleset removes every Machine's filtering until mvmd's next start; deployments keep other firewall management out of mvmd's table.
- A policy cannot name anything nftables cannot match in the kernel, such as a DNS name, without a further mechanism.

## Signs the decision was wrong

- Committing a Machine's chains is a noticeable share of Start or Restore at the node's Machine density.
- Per-packet cost of the chains limits throughput that a tc program would not.
- A guest reaches an address its policy does not allow, or sends with an address that is not its own.
- Guests that a later kernel or workload needs to send other ICMPv6 types (MLD for a multicast workload, router solicitations) fail in ways traced to the tap check.
- Policies need DNS names or other identities that require a proxy, so nftables carries only part of the policy.

## Errata

- 2026-11-09. The decision stands. §Decision and §Context describe "one table" that holds a netdev chain at each tap and inet chains for the policy. A table of nftables has exactly one family: the netlink message that adds a table, and the one that adds a chain to it, each carry a single family (github.com/google/nftables v0.3.0, `table.go` `AddTable` and `chain.go` `AddChain`, which write the table's `Family` into the message header), and the ADR's own context says the `netdev` family has its own hooks and the `inet` family its own. So the netdev chains and the inet chains cannot share a table. It was found while implementing the adapter (#27): mvmd's table is two tables of one name, `mvmd`, a netdev table and an inet table. Both are created, replaced and changed in the same transaction (§Decision, "Changed only by transactions" and "Across restarts"), neither carries the owner flag, and mvmd owns both, so what the decision rests on, one owner and an all-or-nothing change, holds.
