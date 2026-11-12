# ADR 0049: Traffic to a configured no-masquerade prefix keeps the Machine's source address, and runtime info reports the Machine prefixes

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#436, from garamsh/aurora#749 (gaon's decisions: garamsh/gaon#117 D6).

[ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md) masquerades every packet that leaves the node for an address outside the node's prefixes, and lists as a sign the decision was wrong that "masquerade at the node hides source addresses that a policy elsewhere needs to see". gaon is that case: a Machine's ingress policy names its peers on other nodes by prefix ([ADR 0013](./0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md)), which a masqueraded source never matches. gaon also needs the prefixes the node assigns Machine addresses from, which only mvmd's configuration states.

Candidates weighed:

- **Turn masquerade off for the node.** Every destination then sees the Machine's address, including the internet, where an address of the node's prefix is not routable. The exception has to be the cluster's prefixes only.
- **A flag per Machine or per policy rule.** The source address is a property of the node's route to a destination, not of one Machine's policy, and the rule would live in each Machine's chains, which ADR 0012 adds and removes with the Machine.
- **A list of destination prefixes in the node's configuration, one rule each in the base chain** - chosen.

## Decision

1. **`network.no_masquerade` is a list of CIDRs in the node's configuration.** A packet leaving the node to a destination inside one keeps the Machine's source address; to any other destination it is masqueraded as ADR 0011 decides. Empty keeps ADR 0011's behaviour.
2. **Each prefix is checked at load**: a valid CIDR, in a family the node serves. One that is not refuses the start and names it.
3. **The rule is part of mvmd's nftables table** ([ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)): in the inet postrouting chain, one accept for each prefix, ahead of the masquerade rules. It belongs to no Machine and is replaced with the table, in the same transactions.
4. **The policy still decides what leaves.** The exception changes the source address of a packet the Machine's egress rules already allowed; it admits nothing.
5. **`GetRuntimeInfoResponse` gains `machine_prefixes`**: the prefixes the node assigns Machine addresses from, one for each family its `network` section names, in CIDR form, IPv4 first. Empty when the node has no `network` section. The version stays `v1alpha2`: the field is an addition.

## Consequences

- The cluster's other nodes see a Machine's own address, so a Machine's ingress policy can name its peers by prefix. Routing between nodes stays outside mvmd ([runtime.md](../runtime.md) §Network).
- mvmd reads no route table: a listed prefix is not checked against what the node can reach, and one the network does not route loses its return path. The operator's configuration is trusted, as the node's own prefixes are.
- A change to the list needs a restart, as the node's prefixes do.

## Signs the decision was wrong

- A client needs the exception for one Machine or one destination port, so the node-wide list is the wrong grain.
- The prefix set changes often enough that a restart per change is not acceptable.
- A prefix listed here overlaps what a Machine must reach masqueraded, and the exception breaks that traffic.
