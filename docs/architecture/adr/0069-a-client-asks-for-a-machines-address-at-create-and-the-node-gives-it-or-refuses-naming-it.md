# ADR 0069: a client asks for a Machine's address at Create, and the node gives it or refuses naming it

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

mvmd assigns each Machine one address per family its node serves, the lowest free one, at Create Machine ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)). A client learns the address after Create. naru runs Kubernetes control-plane and worker nodes as Machines, and their certificates and kubeconfigs carry the node's address, so the address has to be known before the Machine is made.

## Decision

Create Machine takes an optional address per family, and the node gives the Machine that address in place of the one it would choose.

- **The fields.** `requested_ipv4` and `requested_ipv6` on `CreateMachineRequest`, each an address in text form, empty by default. They are additive in `v1alpha2`, so the version stays. Both forms take them. Restore Machine does not: a restored Machine gets new addresses ([ADR 0016](./0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md)).
- **An address of a bad form is REJECTED** at the API, naming the field and the value: not an address, an IPv6 address in the IPv4 field, or an IPv4 one in the IPv6 field. The inside refuses the same, and an IPv4-mapped IPv6 address and an address with a zone, as invalid.
- **An address outside the node's reach is REJECTED, naming it:** one outside the node's Machine prefix of its family, one in a family the node does not serve, and the prefix's network address and, for IPv4, its broadcast address, which no Machine can have.
- **An address that is taken is CONFLICT, naming it:** the gateway, a resolver, and an address another Machine holds, naming that Machine.
- **Nothing is taken for a refused request.** The Network refuses before it assigns anything, as it does for a routed prefix ([ADR 0061](./0061-a-machine-owns-routed-prefixes-accepted-as-source-only-from-its-tap-and-governed-by-its-policy.md)), and the refusal is the request's, so it is answered as it is and not recorded under the request ID.
- **The address is part of the request.** The Machine's record keeps what was asked, a repeat under the same request ID with the same addresses succeeds, and one with others is a conflict, as for any other parameter. The address is fixed for the Machine's life and released at Delete Machine, with the Machine's others.
- **Without the fields, nothing changes:** the node chooses the lowest free address as before, and skips an address a Machine asked for.
- **What is not decided here.** Whether the address is inside the prefix of one node says nothing of another node's. Placement, and a client's choice among addresses, are the client's.

## Consequences

- A client that picks the address is responsible for picking one no other client picks: the node refuses a second Machine for the same address, and it cannot know which of two clients arrived first for a free one.
- The node's capacity of Machines is unchanged, since a requested address is one of the prefix's.
- A Machine whose address was asked for and a Machine whose address was chosen are the same afterwards: nothing in a record or in Inspect tells them apart, except the record's request.
- The `Network` port's `Allocate` takes what a Machine asks of the network, the MAC and these addresses, in one value.

## Signs the decision was wrong

- A client needs an address for a family at Restore, or a change of a Machine's address, and the fields have to move out of Create.
- Clients collide on addresses often enough that the node should reserve one for a client by name.
