# ADR 0011: Each Machine gets a routed tap and an address mvmd assigns

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

Candidates weighed, for the device:

- **A tap per Machine, created by mvmd and routed by the host** — chosen.
- **macvtap.** It puts the guest on the host's L2 network, outside the host's routing, where the host does not reach the guest. Firecracker attaches only through `/dev/net/tun`.
- **vhost-user-net.** Firecracker does not offer it, and it adds a user-space switch process per node that would itself have to enforce the policy.
- **A bridge joining the taps.** The Machines on a node would share one L2 segment, so one guest's frames reach another's without the host routing them, and enforcement would have to run at the bridge as well as at the route.
- **A network namespace per Machine**, the approach of Firecracker's clone guide. Restored clones need the same tap name only because their device was saved with it; `network_overrides` and Cloud Hypervisor's restore file descriptors remove that need without a namespace per Machine.

And for addressing:

- **mvmd assigns each Machine one IPv4 address from a prefix the node's configuration names, at create and at restore** — chosen.
- **The client assigns the address.** IPAM across nodes is gaon's, and a client-chosen address is an optional field that can be added later without changing what an empty one means.
- **A restored Machine keeps the snapshot's address.** Two Machines restored from one snapshot, or a restore while the snapshotted Machine still exists, would hold one address. Template snapshots (stage v2, `image.md`) restore many clones from one snapshot.
- **DHCP from the host.** A DHCP server per node, for an address mvmd already chose and can send over the channel the guest already listens on.

## Decision

Each Machine's network is one tap device on the host, created by mvmd, with the host as the guest's only next hop. The guest holds one IPv4 address that mvmd assigns from the node's prefix, and the Guest Agent configures it from mvmd's message.

- **The tap.** mvmd creates it persistent and owned by the user the VMM runs as, so the VMM attaches without `CAP_NET_ADMIN`. The link stays down until the Machine's rules are committed ([ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)). The tap exists while the Machine's VMM process exists, and mvmd deletes it after that process has ended.
- **Routed, not bridged.** The host side of each tap carries the gateway address, the same on every tap. The host routes the Machine's address to its tap. No two taps share an L2 segment.
- **The guest's address.** One IPv4 address from the node's prefix, held by the Machine from Create Machine or Restore until Delete Machine, and reported by Inspect. The guest MAC is chosen by mvmd, locally administered, recorded with the Machine and in its snapshots.
- **No IPv6 at v1alpha1.** The guest gets no IPv6 address, and every IPv6 packet from the tap is dropped.
- **Delivered over the Guest Agent channel.** mvmd sends the address, the gateway and the resolvers with the boot description (ADR 0010); the agent configures the interface before running the entrypoint.
- **Restore.** A restored Machine is a new Machine with its own tap and a new address. The Driver attaches the snapshot's network device to the new tap: Firecracker through `network_overrides`, Cloud Hypervisor through the restore's net file descriptors, so its Driver attaches taps by descriptor. The guest keeps the snapshot's MAC. After the restore, mvmd sends the agent the new address, and the agent replaces the old one. Until it does, the guest's packets carry an address its tap does not admit and are dropped. Connections open at the snapshot are lost, as vsock connections are (`guest.md` §Rationale).
- **Leaving the node.** Traffic the policy allows to an address outside the node's prefix leaves with the node's address (masquerade). How Machine addresses are routed between nodes is gaon's, not mvmd's.
- **Resolvers.** The node's configuration names the DNS resolvers mvmd sends the guest. Reaching them is subject to the policy like any other address ([ADR 0013](./0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md)).
- **What the Driver receives.** A network device: the tap's name and the guest MAC. The Driver learns no address and no policy.

## Consequences

- Every packet between a guest and anything else crosses the host's routing, where one mechanism enforces the policy.
- Two Machines restored from one snapshot can share a MAC, which is harmless on separate routed taps.
- The node prefix bounds the Machines a node can hold, and an address is held by a Stopped Machine.
- A Machine's address changes across a restore, and a client that addressed it reads the new one from Inspect.
- The node needs a configured prefix, a gateway address, resolvers and IPv4 forwarding.
- The VMM runs in the host's network namespace; this ADR confines the guest's traffic, not a compromised VMM process.

## Signs the decision was wrong

- gaon needs Machine addresses that survive a move between nodes or a restore, and has to translate them anyway.
- Masquerade at the node hides source addresses that a policy elsewhere needs to see.
- IPv6 is needed by the workloads clients run.
- A Driver for a later VMM cannot attach to a tap, or needs a bridge.
- The time from a restore to the agent's new address is long enough to matter to clients.
