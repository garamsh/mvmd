# ADR 0008: fs-verity on the host guards every read of a MachineImage

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#16 and decided in review of the pull request that adds this ADR.

`image.md`, from garamsh/discussions#2 §5 (discussioncomment-19288321), security: integrity is verified with a root hash the MachineImage carries, and a converted image is never mounted on the host. #16 asks what the root hash covers and who verifies it when: dm-verity or fs-verity, the guest or the host.

What has to be guarded. The registry's bytes are checked by digest when they arrive ([ADR 0007](./0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md), Pull image steps 3 to 5). The EROFS is produced on the node, so it has no digest from outside; once published, it is read block by block by every VMM booting it, for as long as it is cached, from storage that can corrupt or be altered. A whole-file hash at each read is not possible, and a whole-file hash at each boot reads the entire image every time.

From Linux v6.12 `Documentation/`:

- **fs-verity** (`filesystems/fsverity.rst`). `FS_IOC_ENABLE_VERITY` makes the filesystem "build a Merkle tree for the file and persist it"; after it, "the file is made readonly, and all reads from the file are automatically verified against the file's Merkle tree. Reads of any corrupted data, including mmap reads, will fail", with EIO for `read()`. Verity files "cannot be opened for writing or truncate()d". "Direct I/O is not supported on verity files. Attempts to use direct I/O on such files will fall back to buffered I/O." `FS_IOC_MEASURE_VERITY` returns the file's fs-verity digest and "is guaranteed to execute in constant time, regardless of the size of the file". It is "supported by the ext4, f2fs, and btrfs filesystems"; ext4 needs a filesystem formatted with `-O verity` or given it by `tune2fs -O verity`.
- **dm-verity** (`admin-guide/device-mapper/verity.rst`). A read-only device-mapper target over a data device and a hash device, constructed with the root digest, verifying blocks as they are read.

How the VMMs open a read-only disk: Firecracker v1.17.0 opens a drive with write access only when it is not read-only (`src/vmm/src/devices/virtio/block/virtio/device.rs`, `open_file`); Cloud Hypervisor v54.0 does the same (`block/src/factory.rs`, `open_disk`) and adds `O_DIRECT` only when `direct=on`.

Candidates weighed:

- **fs-verity on the host file, measured before each boot** — chosen.
- **dm-verity in the guest**, set up by the Guest Agent from a root hash passed by mvmd. Verification would depend on the guest kernel carrying `dm-verity`, which mvmd does not configure (`guest.md`, guest kernel); the agent would gain device-mapper setup before mounting the root; and the guest it protects is the one running untrusted code.
- **dm-verity on the host**, a loop device and a verity target per image. Host-side as fs-verity is, and independent of the host filesystem, but every image becomes kernel devices to create, track across restarts of mvmd and tear down, and a Driver gets a device path where it would otherwise get a file.
- **A whole-file hash at each boot.** Reads the entire image per boot, and covers nothing read after the check.
- **The registry digest alone.** Covers the source, not the EROFS the node produced or what storage does to it afterwards.

## Decision

The root hash of a MachineImage is the fs-verity digest of its EROFS file, SHA-256 with 4096-byte blocks. The host kernel verifies every read of the file against it; the Image Store checks the file is that one before each boot.

- **What it covers.** Every byte of the EROFS. The rest of the MachineImage (manifest and config) is covered by its content address, checked when it is read.
- **When it is set.** At Pull image step 7, after the converter's VMM has ended and the file is truncated: `FS_IOC_ENABLE_VERITY`, then `FS_IOC_MEASURE_VERITY`, and the digest is recorded in the EROFS descriptor before publish. A file on which it cannot be enabled fails the pull.
- **When it is checked.** Before every start and every restore that boots the image, the Image Store opens the file read-only and measures it. A file that is not a verity file, or whose digest differs from the recorded one, fails the call before the boot begins, with the reason.
- **Who verifies a read.** The host kernel, on every read the VMM makes of the file, whatever I/O path the VMM uses; a corrupted block reaches the guest as a read error.
- **What the host does with the file.** It hashes and measures bytes and never mounts or parses the filesystem.
- **Where the cache lives.** mvmd's state directory is on a filesystem with fs-verity enabled (ext4 with the `verity` feature, f2fs with it, or btrfs). mvmd refuses to start if enabling fs-verity in its image cache fails as unsupported.

## Consequences

- The guest kernel needs nothing for integrity, and the Guest Agent sets up nothing.
- A boot pays a constant-time measurement, not a read of the image.
- A Driver passes a file path as for any disk and needs no integrity vocabulary; a VMM that opens the image with direct I/O gets buffered I/O instead.
- Image files cannot be written once published, by mvmd or a VMM; a disk attached read-write by mistake fails to open rather than altering the image.
- Hosts whose state directory is on XFS or another filesystem without fs-verity cannot run mvmd's image cache.
- The fs-verity digest is a property of the file's bytes and block parameters, so a MachineImage later distributed between nodes can carry the same root hash.
- Integrity is the node's, not the guest's: a privileged host process can still replace a file and its record together, as it can do anything on the node.

## Signs the decision was wrong

- Deployments need a state directory on a filesystem without fs-verity.
- The buffered I/O fs-verity forces costs boot or read latency that direct I/O would not.
- Stage v3 lazy loading fetches chunks of an image that no single local file holds, so fs-verity has nothing to guard and a chunk-level scheme is needed anyway.
- A read error from verification surfaces in the guest in a way that is mistaken for something else, so integrity failures go undiagnosed.
