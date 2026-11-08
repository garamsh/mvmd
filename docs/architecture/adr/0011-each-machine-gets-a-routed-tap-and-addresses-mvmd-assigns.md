# ADR 0011: Each Machine gets a routed tap and addresses mvmd assigns

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#17 and decided in review of the pull request that adds this ADR.

A Machine does not start until its network policy is applied, and the default blocks all external traffic (garamsh/discussions#2 §4, invariant 6 and core and extensions, discussioncomment-19288320). #2 §3 (discussioncomment-19288319) names tenant isolation by "a policy enforced outside the guest". Network and Service schemas, IPAM and cross-node networking are gaon's (#2 §4 lists Network and NetworkPolicy as extensions a Driver interprets; #2 §6 item 10). No container networking stack is used ([ADR 0001](./0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)). The Driver contract is VMM-neutral, and nothing Machine-specific travels on the kernel command line ([ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)).

What the first two VMMs attach a guest network to:

- **Firecracker v1.17.0.** A network interface is a `host_dev_name` with an optional `guest_mac` (`src/firecracker/swagger/firecracker.yaml`, `NetworkInterface`). The device opens `/dev/net/tun` and attaches with `TUNSETIFF` and `IFF_TAP | IFF_NO_PI | IFF_VNET_HDR` (`src/vmm/src/devices/virtio/net/tap.rs`), so it takes a tap by name. vhost-user appears in the API only for block devices (`firecracker.yaml`, `Drive`). On snapshot load, `network_overrides` allows "changing the backing TAP device of a network interface during snapshot restore" (`firecracker.yaml`, `NetworkOverride`). Firecracker's guide for clones states that "each guest will be resumed with the same network configuration, most importantly with the same IP address(es)" (`docs/snapshotting/network-for-clones.md`).
- **Cloud Hypervisor v54.0.** `--net` takes `tap=<if_name>` or `fd=<[fd1,fd2,...]>`, a guest `mac=`, and `vhost_user=` with `socket=` (`vmm/src/config.rs`, `NetConfig::SYNTAX`). A restore can be given new file descriptors for a net device "created with FDs explicitly passed to NetConfig" (`docs/snapshot_restore.md`, Restore a VM with new Net FDs). Its macvtap guide bridges the guest "onto the same L2 network as the host", and "it is not usually possible to reach the guest directly from the host" (`docs/macvtap-bridge.md`).

From Linux v6.12, `Documentation/networking/tuntap.rst`: "CAP_NET_ADMIN is required for creating network devices or for connecting to network devices which aren't owned by the user in question", and persistent devices can be given to unprivileged users.

IPv6 behaviour, from Linux v6.12 `Documentation/networking/ip-sysctl.rst` unless another file is named:

- **Forwarding makes an interface a router.** With IPv6 `forwarding` enabled, "Router behaviour is assumed": "Router Solicitations are not sent unless accept_ra is 2" and "Router Advertisements are ignored unless accept_ra is 2".
- **`accept_ra`** governs accepting Router Advertisements and autoconfiguring from them. Router Solicitations are sent only where they are accepted. `0` is "Do not accept Router Advertisements".
- **`accept_dad`** `0` is "Disable DAD". Separately, an address added with `IFA_F_NODAD` skips duplicate address detection (`include/uapi/linux/if_addr.h`).
- **`addr_gen_mode`** `0`, the default, is to "generate address based on EUI64" for the link-local address; `1` is to "do no generate a link-local address".
- **Neighbour discovery is ICMPv6 and needs hop limit 255.** `ndisc_rcv` drops a neighbour discovery message whose hop limit is not 255 or whose code is not 0. It dispatches router solicitation (133), router advertisement (134), neighbour solicitation (135), neighbour advertisement (136) and redirect (137) (`net/ipv6/ndisc.c`; the numbers are in `include/net/ndisc.h`). Of these, `ndisc.c` has send functions for neighbour advertisements, neighbour solicitations, router solicitations and redirects, and none for router advertisements, which it only receives (`ndisc_router_discovery`): the kernel's neighbour discovery builds no router advertisement.

Candidates weighed, for the device:

- **A tap per Machine, created by mvmd and routed by the host** — chosen.
- **macvtap.** It puts the guest on the host's L2 network, outside the host's routing, where the host does not reach the guest. Firecracker attaches only through `/dev/net/tun`.
- **vhost-user-net.** Firecracker does not offer it, and it adds a user-space switch process per node that would itself have to enforce the policy.
- **A bridge joining the taps.** The Machines on a node would share one L2 segment, so one guest's frames reach another's without the host routing them, and enforcement would have to run at the bridge as well as at the route.
- **A network namespace per Machine**, the approach of Firecracker's clone guide. Restored clones need the same tap name only because their device was saved with it; `network_overrides` and Cloud Hypervisor's restore file descriptors remove that need without a namespace per Machine.

And for addressing:

- **mvmd assigns each Machine one address per family the node is configured for, IPv4 and IPv6, at create and at restore** — chosen.
- **IPv4 only, IPv6 later.** Addressing, the Guest Agent's message and Inspect would each grow a second family later. A client told today that an IPv6 rule is invalid would see it accepted later, which is a change of meaning under the version rule.
- **IPv6 by router advertisement (SLAAC) or DHCPv6 from the host.** Either needs a daemon per node to announce what mvmd has already chosen, since the kernel's neighbour discovery sends no router advertisement. The guest would also accept configuration from a message on the link rather than from mvmd.
- **The client assigns the address.** IPAM across nodes is gaon's, and a client-chosen address is an optional field that can be added later without changing what an empty one means.
- **A restored Machine keeps the snapshot's address.** Two Machines restored from one snapshot, or a restore while the snapshotted Machine still exists, would hold one address. Template snapshots (stage v2, `image.md`) restore many clones from one snapshot.
- **DHCP from the host.** A DHCP server per node, for an address mvmd already chose and can send over the channel the guest already listens on.

## Decision

Each Machine's network is one tap device on the host, created by mvmd, with the host as the guest's only next hop. The guest holds one address per family the node is configured for, assigned by mvmd from the node's prefixes. The Guest Agent configures them statically from mvmd's message.

- **The tap.** mvmd creates it persistent and owned by the user the VMM runs as, so the VMM attaches without `CAP_NET_ADMIN`. The link stays down until the Machine's rules are committed ([ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)). The tap exists while the Machine's VMM process exists, and mvmd deletes it after that process has ended.
- **Routed, not bridged.** The host side of each tap carries one gateway per family, the same on every tap: an IPv4 address from the node's configuration, and the link-local `fe80::1`. mvmd creates the tap with `addr_gen_mode` `1`, so `fe80::1` is the host's only address on it. The host routes each of the Machine's addresses to its tap. No two taps share an L2 segment.
- **Dual stack, per node.** The node's configuration names an IPv4 prefix, an IPv6 prefix, or both. A Machine gets one IPv4 address if the node has an IPv4 prefix, and one IPv6 address (a `/128`) if it has an IPv6 prefix. A node without a family carries none of it: its taps drop that family entirely ([ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)).
- **The guest's addresses.** They are held by the Machine from Create Machine or Restore until Delete Machine, and reported by Inspect. The guest MAC is chosen by mvmd, locally administered, and recorded with the Machine and in its snapshots. The guest's IPv6 link-local address is the EUI-64 one its kernel derives from that MAC by default, which mvmd derives the same way, so the tap check knows it.
- **Static, not advertised.** The host runs no router advertisement or DHCP service. The host's taps forward, so as routers they ignore router advertisements. The agent sets `accept_ra` `0` and `accept_dad` `0` on the guest interface before it comes up, then adds the IPv6 address with `IFA_F_NODAD` and a default route via `fe80::1`. No message on the link configures the guest, and the address is usable at once.
- **Delivered over the Guest Agent channel.** mvmd sends the addresses, the gateways and the resolvers with the boot description (ADR 0010); the agent configures the interface before running the entrypoint.
- **Restore.** A restored Machine is a new Machine with its own tap and new addresses. The Driver attaches the snapshot's network device to the new tap: Firecracker through `network_overrides`, Cloud Hypervisor through the restore's net file descriptors, so its Driver attaches taps by descriptor. The guest keeps the snapshot's MAC. After the restore, mvmd sends the agent the new addresses, and the agent replaces the old ones. Until it does, the guest's packets carry addresses its tap does not admit and are dropped. The IPv6 link-local address follows the MAC and stays the same. Connections open at the snapshot are lost, as vsock connections are (`guest.md` §Rationale).
- **Leaving the node, both families masqueraded.** Traffic the policy allows to an address outside the node's prefixes leaves with the node's address of that family. IPv6 is masqueraded like IPv4, not routed, for two reasons. Whether the node's IPv6 prefix is reachable beyond the node is the network's and gaon's to arrange, which mvmd cannot know. And one rule for both families means a Machine's traffic looks the same to the world on every node. How Machine addresses are routed between nodes is gaon's, not mvmd's.
- **Resolvers.** The node's configuration names the DNS resolvers mvmd sends the guest. Reaching them is subject to the policy like any other address ([ADR 0013](./0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md)).
- **What the Driver receives.** A network device: the tap's name and the guest MAC. The Driver learns no address and no policy.

## Consequences

- Every packet between a guest and anything else crosses the host's routing, where one mechanism enforces the policy.
- Two Machines restored from one snapshot can share a MAC, which is harmless on separate routed taps.
- The node's IPv4 prefix bounds the Machines a dual-stack or IPv4 node can hold, and an address is held by a Stopped Machine.
- A Machine's addresses change across a restore, and a client that addressed it reads the new ones from Inspect.
- The node needs a prefix and forwarding for each family it serves, an IPv4 gateway address if it serves IPv4, and resolvers.
- The guest kernel needs IPv6 built in for a Machine on a node that serves it.
- The VMM runs in the host's network namespace; this ADR confines the guest's traffic, not a compromised VMM process.

## Signs the decision was wrong

- gaon needs Machine addresses that survive a move between nodes or a restore, and has to translate them anyway.
- Masquerade at the node hides source addresses that a policy elsewhere needs to see.
- IPv6 masquerade breaks workloads that need their own address seen, so deployments route the node's IPv6 prefix and want masquerade off.
- Guests need configuration only a router advertisement carries.
- A Driver for a later VMM cannot attach to a tap, or needs a bridge.
- The time from a restore to the agent's new address is long enough to matter to clients.
