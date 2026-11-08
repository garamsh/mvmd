# Image

How mvmd turns an OCI image into something a microVM boots, and where it keeps the result.

Checked against garamsh/discussions#2 as read on 2026-11-08, §5 image section (discussioncomment-19288321), and the decision-change reply (discussioncomment-19466961). The staged plan below is #2's proposal, adopted here as scope and not as a schedule.

## Current decisions

- **mvmd pulls from registries itself and converts to MachineImage, without a container engine.** The OCI image format is the input and distribution format only ([ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).
- **The node understands one format, MachineImage.** It is distributed as an OCI artifact, so existing registries and signing tools apply. It carries run configuration (entrypoint, environment, user, working directory, architecture, required kernel conditions), the root filesystem as EROFS chunks with a chunk index, an integrity root hash, an optional boot prefetch profile, and an optional kernel; with none, the platform default kernel is used (#2 §5, MachineImage proposal).
- **Images arrive by conversion or, later, by a builder of mvmd's own.** Either way the node sees a MachineImage.
- **A running VM's snapshot is a node-local cache, not a distribution source.** Template snapshots are derived per (image, kernel, VMM version, CPU family) combination and are never distributed, because they are less portable, large, and constrained by security (#2 §5).
- **Efficiency comes from the format**: chunk sharing, lazy loading, prefetch, DAX-friendly layout and template snapshots. A bespoke format does not change execution speed by itself (#2 §5).
- **Security rules.** Untrusted images are converted inside isolation, microVM where possible. A converted image is never mounted on the host. Integrity is verified with the root hash (#2 §5, security).
- **Stages (#2 §5, stage plan).** v1: one image to a single EROFS. v2: template snapshots derived on the node as a cache. v3: chunk deduplication, prefetch, lazy loading. v4: a builder of mvmd's own, and images with a kernel.
- **Built by mvmd versus reused.** Reused: registry pull, and a tar to EROFS conversion tool (`mkfs.erofs` or the nydus family). Built: the node image cache and its garbage collection, the guest init and agent ([guest.md](./guest.md)), and guest kernel build and management, which is its own project ([ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).

## Rationale

- Why not run the image as a container: no container runtime anywhere in mvmd ([ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).
- Why a snapshot is cache and not source: #2 §5, image section.
- The Image Store is a driven port with one implementation at first ([structure.md](./structure.md) §Ports).

## Open questions

- How whiteout entries are handled by the chosen conversion tool; #2 §5 marks this as needing a check.
- Which boot formats each Driver accepts for a MachineImage ([driver.md](./driver.md) §Open questions).
- Whether and how the required kernel conditions of an image are negotiated with the guest kernel project once it exists ([guest.md](./guest.md) §Open questions).
