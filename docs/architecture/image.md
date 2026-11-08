# Image

How mvmd turns an OCI image into something a microVM boots, and where it keeps the result.

Checked against garamsh/discussions#2 as read on 2026-11-08, §5 image section (discussioncomment-19288321), and the decision-change reply (discussioncomment-19466961). The staged plan below is #2's proposal, adopted here as scope and not as a schedule. Claims about a tool or a specification are the ADRs', at the versions each names.

## Current decisions

- **mvmd pulls from registries itself and converts to MachineImage, without a container engine.** The OCI image format is the input and distribution format only ([ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).
- **The node understands one format, MachineImage.** It is distributed as an OCI artifact, so existing registries and signing tools apply. It carries run configuration (entrypoint, environment, user, working directory, architecture, required kernel conditions), the root filesystem as EROFS chunks with a chunk index, an integrity root hash, an optional boot prefetch profile, and an optional kernel; with none, the platform default kernel is used (#2 §5, MachineImage proposal).
- **Images arrive by conversion or, later, by a builder of mvmd's own.** Either way the node sees a MachineImage.
- **A running VM's snapshot is a node-local cache, not a distribution source.** Template snapshots are derived per (image, kernel, VMM version, CPU family) combination and are never distributed, because they are less portable, large, and constrained by security (#2 §5).
- **Efficiency comes from the format**: chunk sharing, lazy loading, prefetch, DAX-friendly layout and template snapshots. A bespoke format does not change execution speed by itself (#2 §5).
- **Security rules.** Untrusted images are converted inside isolation, microVM where possible. A converted image is never mounted on the host. Integrity is verified with the root hash (#2 §5, security).
- **Stages (#2 §5, stage plan).** v1: one image to a single EROFS. v2: template snapshots derived on the node as a cache. v3: chunk deduplication, prefetch, lazy loading. v4: a builder of mvmd's own, and images with a kernel.
- **Built by mvmd versus reused.** Reused: the registry protocol, and `mkfs.erofs` from erofs-utils as the tar to EROFS conversion tool ([ADR 0006](./adr/0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md)). Built: the registry client, the converter program and its initrd, the node image cache and its garbage collection, the guest init and agent ([guest.md](./guest.md)). Guest kernel build and management is its own project ([ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).

### Conversion

- **Where it runs.** One converter microVM per pull, booted from the platform guest kernel and mvmd's converter initrd, with no root filesystem and no network device, through the Driver the node's configuration names for conversion ([ADR 0005](./adr/0005-untrusted-images-are-converted-in-a-converter-microvm.md)).
- **What the host does.** It fetches every blob by digest and checks its size and SHA-256, and parses only the JSON of manifests, indexes and configs. It never decompresses or applies a layer.
- **What the converter does.** It receives the verified layers in manifest order over vsock, writes one EROFS to its output disk, and reports the image's length. Its VMM runs under an `mvmd-supervisor` whose expiry is the node's conversion time limit; vCPUs, memory, output size and time are node configuration.
- **How layers are applied.** One `mkfs.erofs` run per layer, the first a full build and each later one incremental, with OCI whiteouts and opaque whiteouts applied against the layers below and removed from the result. `tar+gzip` is filtered by `mkfs.erofs`; `tar+zstd` is decompressed by the converter; other layer types fail the pull ([ADR 0006](./adr/0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md)).
- **A same-layer whiteout** of a name the layer also holds is rewritten by the converter before `mkfs.erofs` sees it: into an opaque marker when the layer makes the name a directory, dropped otherwise. The result matches the OCI layer specification in either order. The converter reads each layer twice, once for its names and once to convert it, with no copy kept (ADR 0006).
- **The output is untrusted.** Only the guests of that image's Machines interpret it; the host reads it as bytes.

### Node image cache

- **Layout.** One OCI image layout, `images/` under mvmd's state directory ([ADR 0007](./adr/0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md)). Blobs are content-addressed by SHA-256, and identical blobs are stored once.
- **A MachineImage** is an artifact manifest with mvmd's MachineImage type, a config blob (run configuration, the converter it came from, required kernel conditions, none at stage v1) and one EROFS blob whose descriptor carries the fs-verity digest. At stage v1 the format is node-local; the media type names are defined with the code.
- **Visibility.** An image exists exactly when `index.json` lists it, under the digest it was pulled by. A pull stages under `images/staging/` and publishes by moving its blobs into place and then replacing `index.json` in one rename. Pulls of one digest are one pull.
- **Start-up sweep.** Before serving any call, mvmd removes staging directories and blobs no listed manifest reaches.
- **The source image is not kept** after conversion.

### Pull image

The steps and the state each failure leaves are in [ADR 0007](./adr/0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md) §Pull image. In order: check the reference names a digest; return if the image is listed; fetch and check the manifest, selecting the node's platform from an index; fetch and check the config; fetch and check each layer; convert; truncate, hash and enable fs-verity; write the config and manifest; publish; remove staging. Nothing before the publish changes `index.json`, so every failure leaves nothing visible in the cache (`runtime-api.md` §Images).

### Integrity

- **The root hash** of a MachineImage is the fs-verity digest of its EROFS file, SHA-256 over 4096-byte blocks ([ADR 0008](./adr/0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md)). The manifest and config are covered by their content addresses.
- **Set** when a pull enables fs-verity on the output, after the converter's VMM has ended and before publish.
- **Checked** before every start and restore that boots the image: the Image Store measures the file, in constant time, and a file that is not a verity file or carries another digest fails the call before the boot begins.
- **Verified on every read** by the host kernel, whatever I/O the VMM uses; a corrupted block reaches the guest as a read error. The guest does nothing for integrity.
- **Host filesystem.** mvmd's state directory is on ext4 or f2fs with the `verity` feature, or on btrfs; mvmd refuses to start where fs-verity cannot be enabled in its image cache.

## Rationale

- Why not run the image as a container: no container runtime anywhere in mvmd ([ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)).
- Why a snapshot is cache and not source: #2 §5, image section.
- Why a microVM and not a host sandbox, and why the host parses only JSON: [ADR 0005](./adr/0005-untrusted-images-are-converted-in-a-converter-microvm.md).
- Why one `mkfs.erofs` run per layer, and how the tool was checked for whiteouts: [ADR 0006](./adr/0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md).
- Why an OCI image layout and one index swap: [ADR 0007](./adr/0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md).
- Why fs-verity on the host and not dm-verity in the guest: [ADR 0008](./adr/0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md).
- The Image Store, the Registry and the Converter channel are driven ports ([structure.md](./structure.md) §Ports); the application sequences a pull across them.

## Open questions

- **Removal of images**: garbage collection of listed images and how a client removes one (`runtime-api.md` §Open questions).
- **Reconversion** when the converter changes: each image records its converter, and nothing yet decides when an image converted by an older one is replaced.
- **Distribution of MachineImages** between nodes and registries, and the published media types it needs.
- Whether and how the required kernel conditions of an image are negotiated with the guest kernel project once it exists ([guest.md](./guest.md) §Open questions).
