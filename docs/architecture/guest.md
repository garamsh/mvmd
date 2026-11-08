# Guest

The Guest Agent that runs inside each Machine, the channel mvmd reaches it by, and the boot order of a Machine.

Checked against garamsh/discussions#2 as read on 2026-11-08, §2 (discussioncomment-19288318), §5 (discussioncomment-19288321) and the decision-change reply (discussioncomment-19466961), and the guest kernel reply (discussioncomment-19466922). Claims about a VMM or a kernel configuration are ADR 0010's, at the versions it names.

## Current decisions

- **The Guest Agent is the guest's init.** It runs as PID 1 from the initrd, prepares the guest, then runs the entrypoint and stays to supervise it. It serves exec and the file API (#2 §5, guest boot order; §2, file operations through the guest agent API).
- **mvmd reaches it over vsock.** The Guest Agent channel is a driven port ([structure.md](./structure.md) §Ports).
- **Boot order (#2 §5):** kernel, init in the initrd, check the root filesystem device, mount the EROFS read-only, overlay the writable disk as the upper layer, switch root, apply the run configuration, run the entrypoint.
- **How the agent finds its devices.** mvmd sends it, over the Guest Agent channel, the filesystem UUIDs of the MachineImage's EROFS and of the root disk, and the run configuration. The agent mounts the block device carrying each UUID; nothing Machine-specific is on the kernel command line, and the boot waits for mvmd's message ([ADR 0010](./adr/0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)).
- **The network.** mvmd sends the agent the Machine's addresses (IPv4, IPv6 or both, as its node serves), the gateways and the resolvers with the boot description. The agent configures the interface statically before the entrypoint runs: `accept_ra` and `accept_dad` off before the link comes up, the IPv6 address without DAD, and the default routes via the IPv4 gateway and `fe80::1`. After a restore, mvmd sends the new Machine's addresses and the agent replaces the old ones ([ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)).
- **The overlay.** Lower: the EROFS, holding no whiteouts or opaque marks ([image.md](./image.md) §Conversion). Upper and work directories: on the root disk, an ext4 filesystem the host formatted ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)).
- **Integrity is not the guest's.** The host kernel verifies every read of the EROFS file ([image.md](./image.md) §Integrity), so the agent sets up no verity device.
- **The host never mounts a guest disk.** A host kernel would interpret an untrusted filesystem, and a second writer would be possible (#2 §2, and §5 security). The host formats a root disk once, before any guest has written it, and reads nothing from it afterwards.
- **Language: Rust**, because one runs per VM and memory and start time matter (#2 §5, implementation language).
- **Guest kernel.** Build and management are a separate project, created at the prototype stage when mvmd first needs to build a kernel itself. The reasons are the GPLv2 source-distribution obligation and a build toolchain and update cadence different from mvmd's. Until then mvmd holds no kernel configuration or patches (guest kernel reply). The kernel conditions the Guest Agent requires are stated explicitly (#2 §5).
- **The kernel is a pinned input.** The node's configuration names one kernel file per architecture and its SHA-256; mvmd checks the digest before every boot, converter included, and records it in inspect and in each snapshot. On x86_64 it is an uncompressed ELF kernel with a PVH entry, on aarch64 a PE `Image` ([ADR 0010](./adr/0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)).
- **Kernel conditions**, built in since the initrd carries no modules: `CONFIG_BLK_DEV_INITRD`, `CONFIG_VIRTIO_BLK`, `CONFIG_VIRTIO_VSOCKETS`, the virtio transport of the Driver's VMM, `CONFIG_EROFS_FS`, `CONFIG_OVERLAY_FS`, `CONFIG_EXT4_FS`, and on x86_64 `CONFIG_PVH`. The agent's own code adds to them when it exists (ADR 0010). For a node that serves IPv6, `CONFIG_IPV6` ([ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)).
- **The guest kernel project is due.** The kernel configurations Firecracker v1.17.0 and Cloud Hypervisor's `ch-7.2.8` branch publish leave EROFS out, so the first real Machine needs a kernel built for it, which is the trigger the guest kernel reply set. Until the project delivers one, a kernel meeting the conditions is supplied to the node from outside mvmd (ADR 0010).

## Rationale

- Why a Rust agent in a Go project: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md); the cost of two languages is acknowledged in #2 §5.
- Why exec and files go through the agent and not the host: #2 §2.
- Why devices are found by filesystem UUID and the kernel is pinned by digest, and the kernel configurations checked: [ADR 0010](./adr/0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md).
- Restore concerns the agent must address after a snapshot restore: vsock connections are reset (Firecracker resets them on restore), the guest clock jumps, and cloned guests share random state (#2 §2).

## Open questions

- **Guest kernel management and the allowed scope of user-supplied kernels (#2 §6 item 11).** Not decided. The guest kernel project does not exist yet; its creation is due (Current decisions).
- How the agent reseeds randomness and corrects time after a restore, and what it does with work interrupted by a pause (#2 §2 lists these as problems to solve).
