# ADR 0013: A network policy is an allow-list of address, protocol and port rules

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#17 and decided in review of the pull request that adds this ADR.

`runtime-api.md` §Open questions left how the network policy is described in Create Machine. The proto's `NetworkPolicy` is an empty placeholder, and an empty policy blocks all external traffic (`lib/runtimeapi/proto/mvmd/runtime/v1alpha1/runtime.proto`). Under `runtime-api.md` §Scope and version, adding an optional field keeps the version, while changing what a call means does not. Network and NetworkPolicy are gaon's, extensions that a Driver interprets (garamsh/discussions#2 §4, core and extensions, discussioncomment-19288320); the runtime API names nothing of its client (`runtime-api.md` §Scope and version). The enforcement matches addresses, protocols, ports and connection state in the kernel ([ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)).

Candidates weighed:

- **Two allow-lists, egress and ingress, of rules naming a prefix, a protocol and ports** — chosen.
- **Ordered allow and deny rules.** The result depends on order, two policies cannot be merged by concatenation, and an added rule can change what an earlier one meant.
- **Named policies or networks as runtime resources.** They are gaon's resources (#2 §4), and the runtime API would gain lifecycle operations for objects only gaon reasons about. The client resolves its resources into the rules a Machine needs.
- **Selectors over Machines (labels), as in Kubernetes NetworkPolicy.** Machines on mvmd carry no labels, and a selector is resolved against a view of other Machines that is the client's.
- **DNS names in rules.** Enforcement in the kernel matches addresses; a name needs a resolver or proxy that mvmd would run and trust, and a name's addresses change while the rule stands.

## Decision

A `NetworkPolicy` is two lists of rules, egress and ingress, each an allow-list. A packet passes when a rule of its direction matches, and nothing else passes. Both lists empty is the default, which blocks all traffic to and from the Machine.

- **A rule** is a prefix, a protocol and a list of port ranges.
  - **Prefix.** An IPv4 prefix (`a.b.c.d/n`). For egress it is the destination; for ingress, the source. A single address is a `/32`.
  - **Protocol.** TCP, UDP, ICMP or any.
  - **Port ranges.** Each a first and a last port, inclusive, `1` to `65535`. For egress they are destination ports; for ingress, the Machine's own ports. Empty means every port. Only TCP and UDP rules may name ports.
- **Replies are implied.** Packets of a connection an accepted packet opened are accepted in both directions, so a policy names only who opens connections.
- **Validation.** An unparseable or IPv6 prefix, a port outside `1` to `65535`, a range whose first port is above its last, or ports on an ICMP or any rule make the call rejected as invalid, and nothing changes.
- **Where it is given.** Create Machine and Restore carry the policy. A snapshot carries none: the restored Machine is a new Machine with its own policy. In v1alpha1 a Machine's policy is fixed for its life.
- **DNS.** The resolvers are the node's ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-an-address-mvmd-assigns.md)), and a policy that allows them names their addresses on UDP and TCP port 53, like any other rule.
- **Inspect** reports the Machine's address and the policy in effect.
- **What later versions may add without a new version.** New optional fields whose absence means what v1alpha1 means: IPv6 prefixes, name-based rules, an operation that replaces a Machine's policy, and a client-chosen address. A change that makes an empty list mean something other than "nothing passes" is a new version.

## Consequences

- The policy maps one to one onto kernel rules, so what is enforced is what the client sent.
- A client that thinks in names or services resolves them to prefixes before calling, and calls again on a change only once a replace operation exists.
- A Machine that needs to talk to another Machine on the node needs both policies to say so.
- A policy has no deny rule, so a client excludes an address by not including it.

## Signs the decision was wrong

- Clients routinely need a deny inside an allowed prefix, and split prefixes to express it.
- Policies grow to many rules per Machine because the client must enumerate addresses that a name or a selector would cover.
- Machines are deleted and recreated, or restored, only to change their policy.
