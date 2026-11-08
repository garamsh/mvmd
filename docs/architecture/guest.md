# Guest

The Guest Agent that runs inside each Machine, the channel mvmd reaches it by, and the boot order of a Machine.

Checked against garamsh/discussions#2 as read on 2026-11-08, §2 (discussioncomment-19288318), §5 (discussioncomment-19288321) and the decision-change reply (discussioncomment-19466961), and the guest kernel reply (discussioncomment-19466922).

## Current decisions

- **The Guest Agent is the guest's init.** It runs as PID 1 from the initrd, prepares the guest, then runs the entrypoint and stays to supervise it. It serves exec and the file API (#2 §5, guest boot order; §2, file operations through the guest agent API).
- **mvmd reaches it over vsock.** The Guest Agent channel is a driven port ([structure.md](./structure.md) §Ports).
- **Boot order (#2 §5):** kernel, init in the initrd, check the root filesystem device, mount the EROFS read-only, overlay the writable disk as the upper layer, switch root, apply the run configuration, run the entrypoint.
- **The host never mounts a guest disk.** A host kernel would interpret an untrusted filesystem, and a second writer would be possible (#2 §2, and §5 security).
- **Language: Rust**, because one runs per VM and memory and start time matter. The UFFD lazy-restore handler, a page-fault path where garbage-collection pauses become guest latency, is Rust too (#2 §5, implementation language).
- **Guest kernel.** Build and management are a separate project, created at the prototype stage when mvmd first needs to build a kernel itself. The reasons are the GPLv2 source-distribution obligation and a build toolchain and update cadence different from mvmd's. Until then mvmd holds no kernel configuration or patches (guest kernel reply). The kernel conditions the Guest Agent requires are stated explicitly (#2 §5).

## Rationale

- Why a Rust agent in a Go project: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md); the cost of two languages is acknowledged in #2 §5.
- Why exec and files go through the agent and not the host: #2 §2.
- Restore concerns the agent must address after a snapshot restore: vsock connections are reset (Firecracker resets them on restore), the guest clock jumps, and cloned guests share random state (#2 §2).

## Open questions

- **Guest kernel management and the allowed scope of user-supplied kernels (#2 §6 item 11).** Not decided. The guest kernel project does not exist yet.
- The conditions a kernel must meet for the Guest Agent; #2 requires them to be stated and does not state them.
- How the agent reseeds randomness and corrects time after a restore, and what it does with work interrupted by a pause (#2 §2 lists these as problems to solve).
