# ADR 0012: nftables on the host enforces each Machine's policy at its tap

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#17 and decided in review of the pull request that adds this ADR.

`runtime.md` §Open questions left the enforcement technology open, naming nftables and eBPF from garamsh/discussions#2 §6 item 10 (discussioncomment-19288322). The rules it must keep: a Machine does not start until its policy is applied, the default blocks all external traffic (#2 §4, discussioncomment-19288320), and the policy is enforced outside the guest (#2 §3, discussioncomment-19288319). Each Machine's traffic crosses one routed tap ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-an-address-mvmd-assigns.md)).

From nftables 1.1.7's documentation (`doc/nft.txt`, `doc/statements.txt` and `doc/data-types.txt`, as packaged in Debian `nftables` 1.1.7-3):

- The `netdev` family "handles packets from the device ingress and egress path" and filters "packets of any ethertype such as ARP … as well as IPv4 and IPv6 packets". Its ingress hook is invoked "right after *tc* ingress and before layer 3 protocol handlers". Its base chains "require the 'DEVICE' parameter … since they exist per interface only".
- The `inet` family has the `prerouting`, `input`, `forward`, `output` and `postrouting` hooks. `ct state established,related` matches connection-tracking state, and `masquerade` "makes sense only in postrouting".
- A table with the `owner` flag "will be removed when the process exits" unless it also carries `persist`.

From Linux v6.12:

- `net/netfilter/nfnetlink.c`, `nfnetlink_rcv_batch`: the messages of one batch are committed (`ss->commit`) only when the whole batch was processed without failure; otherwise the subsystem aborts (`ss->abort`) and none of the batch takes effect.
- `net/netfilter/Kconfig`: `NETFILTER_INGRESS` and `NETFILTER_EGRESS` default to `y`.

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

- **At the tap (netdev ingress).** Every frame the guest sends is checked against the Machine's MAC and address. ARP is accepted only when it asks for the gateway, from the guest's MAC and address. IPv4 is accepted only from that MAC and address. Everything else, IPv6 included, is dropped. No guest can send as another.
- **The policy (inet).** Forward and input chains accept a packet from a Machine only if an egress rule of its policy matches, and to a Machine only if an ingress rule matches ([ADR 0013](./0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md)). Replies of an accepted connection are accepted by connection-tracking state. Anything else from or to a Machine's tap is dropped, the host's own services included. A packet between two Machines on the node needs the sender's egress and the receiver's ingress.
- **Leaving the node.** A postrouting rule masquerades accepted traffic whose destination is outside the node's prefix (ADR 0011).
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
- Policies need DNS names or other identities that require a proxy, so nftables carries only part of the policy.
