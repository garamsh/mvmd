# ADR 0061: a Machine owns routed prefixes accepted as source only from its tap and governed by its policy

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A Machine holds one address per family its node serves, and its tap admits a frame only if its source is the Machine's MAC and those addresses ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md), [ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)). A guest that is itself a router, or that owns a block of addresses the network routes to the node, has no way to send from them: the tap drops the frames, and the node has no route back. The decision was taken under garamsh/aurora#804 and recorded for garamsh/mvmd#663.

## Decision

A Machine owns zero to four routed prefixes besides its addresses. This amends ADR 0011 (a tap admits only the Machine's own addresses as source) and ADR 0012 (the table's rules are per address), and no more.

- **The field.** `routed_prefixes`, repeated CIDR strings, on `CreateMachineRequest` and `RestoreMachineRequest` and reported on `Machine`. It is additive in `v1alpha2`, so the version stays. A Restore takes the prefixes from its request: a snapshot carries none, as it carries no address or policy.
- **The rules.** At most 4. Each is an IPv4 prefix from /16 to /32 or an IPv6 prefix from /48 to /128, in a family the node serves, with the host bits clear. Loopback, link-local, multicast, unspecified and IPv4-mapped IPv6 space is refused, and so is a prefix that is not a CIDR. A prefix outside these rules is `INVALID_ARGUMENT` (wire `REJECTED`), naming it.
- **No overlap.** A prefix may not overlap the node's Machine prefix of its family, a Machine's address, another Machine's routed prefix, another prefix of the same request, or a resolver the node gives Machines. An overlap with another Machine or the node's own space is `ALREADY_EXISTS` (wire `CONFLICT`), naming the prefix; an overlap inside one request is invalid.
- **Fixed for life.** The prefixes are fixed at Create or Restore, released at Delete, and held by the record, so a restart finds them. A repeat of Create compares them with the rest of the request.
- **What mvmd does.** It routes each prefix to the Machine's tap (an IPv4 route with link scope, an IPv6 route), re-made at Reconcile from the record. The tap's chain admits a source in a routed prefix, from that tap only, and the Machine's policy chains treat the prefixes as the Machine's own, because they are keyed by interface. Masquerade and no-masquerade work as for the Machine's address ([ADR 0049](./0049-traffic-to-a-configured-no-masquerade-prefix-keeps-the-machines-source-address-and-runtime-info-reports-the-machine-prefixes.md)): a routed source leaving the node is masqueraded by a per-Machine postrouting rule, tagged with the tap so removal finds it, except to another Machine's tap. The base masquerade rule skips an output interface named `mvmd*` for the same reason, so a Machine's traffic to another Machine's routed prefix reaches it with its source address.
- **The guest.** The guest is not configured: the Guest Agent adds no address or route for a routed prefix. What the guest does with the block is its own.

## Consequences

- A Machine can be a router for a block, and the node's tap enforcement still stops every other source.
- The routes go with the link, so Withdraw removes none explicitly; the postrouting rules are removed by tag with the rest of the Machine's rules.
- The `Allocate` port takes the prefixes, so the adapter alone decides overlap, the way it alone holds addresses.
- The base masquerade rule's exclusion changes what a Machine's traffic to another Machine's own address looks like only where the output interface is another tap, which the egress and ingress policy chains already decide.
- A prefix of the node's Machine space, or one containing a resolver, is a conflict and not an invalid request, because it depends on what this node and its other Machines hold; a prefix in a family the node does not serve is invalid, as the rule on families says.

## Signs the decision was wrong

- Clients need more than four prefixes, or a prefix shorter than /16 or /48.
- Clients need to change the prefixes of a running Machine, so the fixed-for-life rule costs a replacement.
- A guest is expected to be configured with a routed prefix by the Guest Agent.
- Traffic between two Machines needs the translated source after all, so the oif exclusion is the wrong default.
