# ADR 0007: The node image cache is an OCI image layout, published by one index swap

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#16 and decided in review of the pull request that adds this ADR.

A MachineImage is distributed as an OCI artifact and carries run configuration, the root filesystem as EROFS and an integrity root hash (`image.md`, from garamsh/discussions#2 §5, discussioncomment-19288321). Pull image names a digest, succeeds when the image is already present, and leaves nothing visible in the cache after a failure (`runtime-api.md` §Images). The node cache and its garbage collection are built by mvmd (`image.md`).

From the OCI image specification v1.1.1:

- An image layout is a directory holding `oci-layout`, `index.json` and `blobs/`; `index.json` "MUST be an image index"; "The content of `blobs/<alg>/<encoded>` MUST match the digest `<alg>:<encoded>`" (`image-layout.md`).
- An artifact manifest sets `artifactType`, which "MUST be set when `config.mediaType` is set to the empty value", and implementations "MUST NOT error on encountering an `artifactType` that is unknown" (`manifest.md`).
- An image index selects a manifest per platform (`image-index.md`).

From Linux man-pages 6.9.1: when the target of `rename` exists, "it will be atomically replaced, so that there is no point at which another process attempting to access newpath will find it missing" (`rename(2)`), and `fsync` of a file "does not necessarily ensure that the entry in the directory containing the file has also reached disk. For that an explicit fsync() on a file descriptor for the directory is also needed" (`fsync(2)`).

Candidates weighed:

- **One OCI image layout as the cache, published by replacing `index.json`** — chosen.
- **A directory per image, published by renaming it into place.** Equally atomic, but every image holds its own copies, and a MachineImage pushed later to a registry would need converting into a layout first.
- **A database (an embedded key-value store) of image records.** A second store to keep consistent with the files it describes, for what one JSON index already records.
- **Keeping the source image's layers in the cache.** Nothing on the node reads them after conversion; they would double the cache for a reconversion that a new pull can do.

## Decision

The node image cache is one OCI image layout, `images/` under mvmd's state directory. A MachineImage is an artifact manifest in it. `index.json` is the cache's only record of which images exist: an image is visible exactly when `index.json` lists it.

- **What a MachineImage holds.** An artifact manifest whose `artifactType` is mvmd's MachineImage type, a config blob with the run configuration (entrypoint, command, environment, user, working directory, architecture), the converter it was produced by, and required kernel conditions (none at stage v1), and one layer blob, the EROFS. The EROFS descriptor carries its fs-verity digest as an annotation ([ADR 0008](./0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md)). The media type names are defined with the code; at stage v1 the format is node-local and is published to no registry.
- **Content addressing.** Every blob sits at `blobs/sha256/<hex>` named by its SHA-256, as the layout requires. The index entry for an image records the digest of the reference it was pulled by, which is how Pull image and Create Machine find it.
- **Staging.** A pull works in its own directory under `images/staging/`, outside the layout, which the index never references.
- **Publish.** The pull moves its blobs into `blobs/sha256/` by rename, keeping a blob already present under the same name, `fsync`s them and the directory, then writes a new `index.json` beside the old one, `fsync`s it, renames it over the old one and `fsync`s the directory. Commits are applied one at a time.
- **Pulls of one digest are one pull.** A second Pull image naming a digest already being pulled waits for that pull's outcome rather than starting another.
- **Start-up sweep.** Before serving any call, mvmd removes every staging directory and every blob no manifest listed in `index.json` reaches. Nothing else is removed by this ADR; removal of listed images is garbage collection, which stays open.
- **The source image is not kept.** Its manifest, config and layers live in staging only.

### Pull image

Each step's failure leaves the cache as `runtime-api.md` §Images requires, "Nothing visible in the cache", because nothing before step 9 changes `index.json`.

| Step | What happens | On failure |
|---|---|---|
| 1 | The reference is checked to name a digest. | Rejected as invalid; nothing was written. |
| 2 | `index.json` is read for an image pulled by that digest. | Present: success with no change, the Repeat column of `runtime-api.md` §Images. |
| 3 | The manifest is fetched by digest and checked against it (size and SHA-256). An index selects the manifest for the node's platform, which is fetched and checked the same way; an index with none for it fails the pull. | Failed; the staging directory is removed. |
| 4 | The config is fetched and checked, and the run configuration is decoded from it. | Failed; the staging directory is removed. |
| 5 | Each layer is fetched into staging and checked against its descriptor's size and digest while it streams. A layer media type other than `tar`, `tar+gzip` or `tar+zstd` fails the pull. | Failed; the staging directory is removed. |
| 6 | The converter microVM converts the layers ([ADR 0005](./0005-untrusted-images-are-converted-in-a-converter-microvm.md), [ADR 0006](./0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md)). | Failed, including at the time limit; the supervisor ends the VMM, and the staging directory is removed. |
| 7 | With the VMM ended, the output is truncated to the reported length, `fsync`ed and hashed, and fs-verity is enabled on it and measured ([ADR 0008](./0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md)). | Failed; the staging directory is removed. |
| 8 | The config blob and the manifest are written into staging. | Failed; the staging directory is removed. |
| 9 | Publish, as above. The image is visible once `index.json` is replaced. | Before the replace: failed, and the moved blobs are unreferenced. |
| 10 | The staging directory is removed. | Nothing visible changes; the start-up sweep removes what is left. |

A crash of mvmd at any step leaves the old `index.json` or the new one, never a partial one; what it leaves in staging or as unreferenced blobs is removed by the start-up sweep before any call is served.

## Consequences

- Identical blobs, the same EROFS pulled by two references among them, are stored once.
- A MachineImage on the node is already in the form a registry push takes, when distribution is decided.
- Every publish rewrites `index.json`, and commits are serialised, so publish cost grows with the number of images on the node.
- A crash between moving blobs and replacing the index costs disk space until the next start of mvmd.
- Reconverting an image with a newer converter means removing it and pulling again; nothing reconverts in place.

## Signs the decision was wrong

- `index.json` grows to where rewriting it is a noticeable part of a pull.
- A failed or interrupted pull leaves an image visible, or one mvmd lists that it cannot boot.
- Unreferenced blobs accumulate between restarts enough to matter, so the sweep needs to run while mvmd serves calls.
- A client needs a MachineImage found by something other than the digest it was pulled by.
