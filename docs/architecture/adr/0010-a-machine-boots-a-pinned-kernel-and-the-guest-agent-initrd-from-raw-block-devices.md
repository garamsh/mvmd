# ADR 0010: A Machine boots a pinned kernel and the Guest Agent initrd from raw block devices

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#16 and decided in review of the pull request that adds this ADR.

The Driver contract is stated in VMM-neutral terms ([ADR 0001](./0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md); `driver.md`), and which boot formats each Driver accepts was left to be checked per Driver (garamsh/discussions#2 §5, discussioncomment-19288321). The guest boots a kernel, the Guest Agent from the initrd, then mounts the EROFS read-only under an overlay whose upper layer is the writable disk (`guest.md`). Guest kernel build and management is a separate project, created at the prototype stage "when mvmd first needs to build a kernel itself"; until then mvmd holds no kernel configuration or patches (guest kernel reply, discussioncomment-19466922; ADR 0001).

What the first two VMMs boot:

- **Firecracker v1.17.0.** On x86_64 "uncompressed ELF (`vmlinux`) kernel images as well as `bzImage` images", on aarch64 "PE formatted (`Image`) images"; `bzImage` "is decompressed by the guest" and `vmlinux` "is recommended" (`docs/rootfs-and-kernel-setup.md`). A kernel carrying the `XEN_ELFNOTE_PHYS32_ENTRY` note boots in PVH mode, enabled for Linux by `CONFIG_PVH=y` (`docs/pvh.md`). The boot source is a kernel path, an initrd path and boot arguments (`src/firecracker/swagger/firecracker.yaml`, `BootSource`).
- **Cloud Hypervisor v54.0.** "For x86-64, a `vmlinux` ELF kernel (compiled with PVH support) or a regular bzImage", and an `Image` for AArch64 (`README.md`, Custom Kernel and Disk Image); `--kernel`, `--initramfs` and `--cmdline` (`cloud-hypervisor/src/main.rs`).
- **Naming a disk to the guest.** Firecracker's virtio-blk device ID is built from the backing file's `st_dev`, `st_rdev` and `st_ino` and cannot be chosen ([ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md), Context). Both filesystems a Machine boots carry a UUID mvmd chooses: the EROFS through `mkfs.erofs -U` ([ADR 0006](./0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md)), the root disk through `mke2fs -U` (ADR 0009).

The kernels the two VMM projects publish configurations for:

- Firecracker v1.17.0 `resources/guest_configs/`: `microvm-kernel-ci-x86_64-6.1.config`, `microvm-kernel-ci-x86_64-6.18.config`, `microvm-kernel-ci-aarch64-6.1.config` and `microvm-kernel-ci-aarch64-6.18.config` each read `# CONFIG_EROFS_FS is not set`; they set `CONFIG_OVERLAY_FS=y`, `CONFIG_EXT4_FS=y`, `CONFIG_VIRTIO_BLK=y` and `CONFIG_VIRTIO_VSOCKETS=y`, and the x86_64 ones `CONFIG_PVH=y`. Firecracker states the configurations "should be used to build exclusively the aforementioned Amazon Linux kernels" (`docs/kernel-policy.md`).
- Cloud Hypervisor's kernel branch `ch-7.2.8` of `cloud-hypervisor/linux`, at commit 639d0886248cd90799b0b590f7a62a1a3565a815, `arch/x86/configs/ch_defconfig`: no `CONFIG_EROFS_FS` entry, and `CONFIG_PVH=y`, `CONFIG_OVERLAY_FS=y`, `CONFIG_EXT4_FS=y` and `CONFIG_VIRTIO_VSOCKETS=y`.

Neither boots an EROFS root filesystem.

Candidates weighed, for the boot artefacts:

- **An uncompressed ELF kernel with a PVH entry on x86_64, a PE `Image` on aarch64; the Guest Agent's initrd; raw block devices the guest finds by filesystem UUID** — chosen.
- **`bzImage` on x86_64.** Both VMMs take it, and the guest pays its decompression at every boot.
- **Finding disks by their order.** The guest would depend on each VMM's device enumeration, which this ADR did not find stated as an interface in either VMM's documentation at those versions.
- **Finding disks by virtio-blk serial.** Firecracker's cannot be chosen.
- **The run configuration and the disk UUIDs on the kernel command line.** The environment is unbounded client input, the kernel parses the command line too, and a Driver may add to it; the Guest Agent channel carries them without either constraint.

And for the kernel:

- **A kernel file the node's configuration names by path and SHA-256, built outside mvmd** — chosen.
- **The kernels Firecracker's CI builds.** They lack EROFS, and Firecracker scopes their configurations to Amazon Linux kernels.
- **mvmd building the kernel.** ADR 0001 places kernel build in a separate project.
- **A distribution's kernel.** Not checked against any distribution's configuration; it is a candidate for the guest kernel project to evaluate.

## Decision

A Driver boots a VM from boot artefacts stated in mvmd's terms: a kernel file, an initrd file, a kernel command line, an ordered list of raw block devices each named by a file path and a read-only flag, one vsock device, and the vCPU count and memory size. The kernel is a file the node's configuration pins by digest; mvmd builds none.

- **Kernel format.** On x86_64 an uncompressed ELF kernel with a PVH entry; on aarch64 a PE `Image`. Both Drivers take each as it is.
- **A Machine's devices.** The MachineImage's EROFS file read-only, the root disk read-write, then the disks attached to it. The initrd is the Guest Agent's.
- **The converter's devices.** The output disk read-write and nothing else; the initrd is the converter's ([ADR 0005](./0005-untrusted-images-are-converted-in-a-converter-microvm.md)).
- **How the guest finds them.** mvmd sends the Guest Agent, over the Guest Agent channel, the UUIDs of the image's and the root disk's filesystems and the run configuration. The agent mounts the device carrying each UUID. Nothing Machine-specific travels on the kernel command line.
- **The command line** carries mvmd's guest parameters; a Driver may add the ones its VMM's devices need and removes none.
- **The kernel input.** The node's configuration names one kernel file per architecture and its SHA-256. mvmd checks the digest before every boot, the converter's included, refuses a boot on a mismatch, and records the digest in inspect and in every snapshot as the guest kernel (`driver.md`, snapshot compatibility).
- **Kernel conditions.** Built in, since the initrd carries no modules: initrd support (`CONFIG_BLK_DEV_INITRD`), virtio block (`CONFIG_VIRTIO_BLK`), virtio vsock (`CONFIG_VIRTIO_VSOCKETS`), the virtio transport the Driver's VMM presents, EROFS (`CONFIG_EROFS_FS`), overlayfs (`CONFIG_OVERLAY_FS`) and ext4 (`CONFIG_EXT4_FS`); on x86_64, `CONFIG_PVH`. The Guest Agent adds what its own code needs when that code exists.
- **The guest kernel project is due.** No kernel either VMM project publishes a configuration for meets these conditions, so the first real Machine needs a kernel built for it. That is the trigger the guest kernel reply set. Until the project delivers one, a kernel meeting the conditions is supplied to the node from outside mvmd, by path and digest.

## Consequences

- A Driver translates the artefacts into its VMM's API and knows nothing of images, overlays or the Guest Agent.
- The same kernel file serves both Drivers on an architecture.
- The guest's boot waits for mvmd's first message on the Guest Agent channel; a Machine cannot finish booting without mvmd reachable.
- A node without a configured kernel that matches its digest boots nothing, conversions included.
- The first real Machine depends on a kernel built outside mvmd, which the guest kernel project or an operator provides.
- Kernel conditions are now stated, which `guest.md` had left open.

## Signs the decision was wrong

- A Driver needs to know which device is the image or the root disk to boot it.
- A VMM admitted later cannot boot a PVH ELF kernel or an `Image` and needs a per-Driver kernel.
- The guest kernel project chooses a kernel these conditions do not describe, or finds a published kernel that meets them, so the project was not needed for the first Machine.
- Booting stalls or fails when mvmd is slow to reach the Guest Agent at boot, so the boot description belongs in the guest before the channel exists.

## Errata

- 2026-11-11. The decision stands. §Context, §Decision (The guest kernel project is due) and §Consequences call the owner of the guest kernel "a separate project" and "the guest kernel project", created at the prototype stage. Decision 6 of [garamsh/discussions#3](https://github.com/garamsh/discussions/discussions/3), the owner's, read on 2026-11-11, replaces the guest kernel reply this ADR cites: the project that builds and manages the guest kernel is naru, and the rule that mvmd holds no kernel configuration or patches is unchanged. Where this ADR says "the guest kernel project", it means that project. The kernel is still a file the node's configuration pins by path and digest, and mvmd builds none.
