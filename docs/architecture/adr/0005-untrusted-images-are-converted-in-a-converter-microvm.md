# ADR 0005: Untrusted images are converted in a converter microVM

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#16 and decided in review of the pull request that adds this ADR.

`image.md` fixed the direction from garamsh/discussions#2 §5 (discussioncomment-19288321), security: untrusted images are converted inside isolation, a microVM where possible, and a converted image is never mounted on the host. It left open where conversion runs and how mvmd boots that VM before any user image exists. Constraints:

- No container, container runtime or OCI runtime anywhere in mvmd ([ADR 0001](./0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).
- Every VMM process runs under an `mvmd-supervisor` that holds an expiry ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)).
- Pull image leaves nothing visible after a failure (`runtime-api.md` §Images).

What is untrusted in a pull, by the OCI image specification v1.1.1 and the OCI distribution specification v1.1.1:

- **Descriptors are checkable on the host without interpreting content.** "Retrieved content SHOULD be verified against this digest when consumed via untrusted sources", content whose length differs from `size` "SHOULD NOT be trusted", and implementations "MUST implement SHA-256 digest verification for use in descriptors" (image-spec `descriptor.md`). For manifests and blobs alike, a client fetching by digest "SHOULD verify" the response matches it (distribution-spec `spec.md`, Pulling manifests and Pulling blobs). Hashing bytes interprets nothing.
- **Manifests, indexes and configs are JSON**, small, and needed on the host to know what to fetch and how to run it.
- **Layers are tar archives**, optionally gzip- or zstd-compressed (image-spec `layer.md`, media types), whose entries are applied in order with whiteouts. Decompressing and applying them is the work that interprets attacker-shaped structure, and the tool that does it, `mkfs.erofs`, is C ([ADR 0006](./0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md)).

Candidates weighed:

- **A converter microVM per pull, booted from mvmd's own converter initrd** — chosen.
- **`mkfs.erofs` as a host process in a sandbox** (unprivileged user, seccomp, namespaces). The C parser of attacker-shaped input would run in the host kernel's user space, with the sandbox as the only barrier; a sandbox of namespaces and syscall filters is the isolation unit of a container, which ADR 0001 excludes as an isolation unit.
- **Applying the layers in the Machine's own guest at first boot.** No EROFS and no integrity root exists before the first boot, every Machine of the image repeats the work, and the node cache holds nothing a second Machine can share.
- **A remote conversion service.** It moves the trust boundary off the node to a service mvmd does not own; the builder of stage v4 (#2 §5, stage plan) is where off-node production of MachineImages belongs.
- **Converting inside a Machine of a trusted "converter image".** A Machine needs a MachineImage, so the first one could not be produced; the converter has no use for a root filesystem, an overlay or the Guest Agent's file API.

## Decision

Each pull that converts an image runs one converter microVM. It boots the platform guest kernel and mvmd's converter initrd, which holds the converter program and the conversion tool, and has no root filesystem. The host fetches and verifies every blob by digest and parses only JSON; the converter alone decompresses and applies layers.

- **What crosses into the converter.** The verified layer blobs, in manifest order, streamed over vsock, and the parameters mvmd chose (the filesystem UUID among them, [ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)). The converter has no network device, so it reaches no registry and nothing else.
- **What comes out.** One raw output block device, backed by a sparse file in the pull's staging directory and sized at the node's configured maximum image size, onto which the converter writes the EROFS; and a report over vsock of success and the image's length. The host truncates the file to that length. It reads the output only as bytes ([ADR 0008](./0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md)), never as a filesystem.
- **How it is booted before any user image exists.** From the converter initrd and the platform guest kernel ([ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)), through the VMM Driver the node's configuration names for conversion. It needs no optional Driver capability.
- **Supervised like any VMM.** Its VMM runs under an `mvmd-supervisor` whose expiry is the start of the conversion plus the node's configured conversion time limit, so a hung or hostile conversion ends at the limit whether or not mvmd is running. The fencing deadline does not apply: a converter runs no workload, and Pull image does not need the deadline armed (`runtime-api.md` §Images).
- **Bounded.** vCPUs, memory, output size and time are node configuration; exhausting any of them fails the pull.
- **Built by mvmd.** The converter program and the build of its initrd are the `imageconv/` unit, in Rust under `docs/convention/stack-rust.md`, for the reasons the Guest Agent is (#2 §5, implementation language): it runs per VM, where memory and start time matter.
- **Orchestrated by the inside.** The application sequences the pull across ports: the registry fetch, the cache's staging and commit, the Driver and the supervisor for the VM, and the conversation with the converter. No adapter calls another (`structure.md` §Boundaries and dependency direction).

## Consequences

- A bug in the conversion tool that an image exploits reaches a VM holding only that pull's layers and output, and no network. The worst result is a wrong or hostile EROFS for that one image, which only that image's Machines mount, in their own guests.
- Every conversion pays a VM boot.
- A converted EROFS is untrusted output: the guest kernel of each Machine of that image interprets it, as it interprets the workload, and nothing on the host does.
- mvmd builds and ships a second initrd, and the conversion tool is pinned inside it rather than taken from the host.
- Pull image needs a Driver in service and the guest kernel, so a node that cannot boot a VM cannot pull.
- Two driven ports appear, the registry and the converter channel (`structure.md` §Ports).

## Signs the decision was wrong

- Conversion time is dominated by the VM's boot rather than by the conversion, for the images clients pull.
- The converter needs a capability a Driver must negotiate, or a VMM-specific path, to run at all.
- A conversion escapes its limits in a way the VM's bounds did not stop, such as host memory pressure from the VMM process itself.
- Parsing a manifest, index or config on the host turns out to need more than JSON decoding, so attacker-shaped structure is interpreted on the host anyway.
