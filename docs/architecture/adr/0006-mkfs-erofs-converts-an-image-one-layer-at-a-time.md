# ADR 0006: mkfs.erofs converts an image one layer at a time

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#16 and decided in review of the pull request that adds this ADR.

Stage v1 converts one image to a single EROFS (`image.md`, stages). garamsh/discussions#2 §5 (discussioncomment-19288321) names the tools to reuse, `mkfs.erofs` or the nydus family, and marks how each handles whiteouts as needing a check. The conversion runs inside the converter microVM ([ADR 0005](./0005-untrusted-images-are-converted-in-a-converter-microvm.md)).

What a correct result is, from the OCI image specification v1.1.1:

- The `layers` array has "the base layer at index 0", and the rest follow "in stack order" (`manifest.md`).
- A whiteout is an entry whose basename is `.wh.` plus the name to delete; "Whiteout files MUST only apply to resources in lower/parent layers", "Files that are present in the same layer as a whiteout file can only be hidden by whiteout files in subsequent layers", and "Once a whiteout is applied, the whiteout itself MUST also be hidden". An entry named `.wh..wh..opq` hides all children of its directory in the lower layers (`layer.md`, Whiteouts and Opaque Whiteout).
- Layer media types are `tar`, `tar+gzip` and `tar+zstd`, distributable or not (`layer.md`).

What `mkfs.erofs` does, from erofs-utils v1.9.4 (`man/mkfs.erofs.1` and the source files named):

- `--tar` treats the source as a tarball, read from standard input when no source is named (`mkfs/main.c`); `--ungzip` and `--unxz` filter the stream. No zstd filter exists among the options (`mkfs/main.c`, long options).
- `--aufs` "Replace[s] aufs special files with overlayfs metadata": in `lib/rebuild.c` (`erofs_rebuild_get_dentry`) and `lib/tar.c` (`tarerofs_parse_tar`), an entry `.wh.<name>` becomes an overlayfs whiteout (a character device 0/0, the format `Documentation/filesystems/overlayfs.rst` of Linux v6.12 defines) at `<name>`, replacing an entry of that name already in the tree, and `.wh..wh..opq` marks its directory opaque.
- `--incremental=data` appends a tar source to an existing EROFS. While building a directory, `lib/inode.c` loads the base image's entries for it only when the directory is not opaque, and `erofs_rebuild_basedir_dirent_iter` in `lib/rebuild.c` keeps an entry of the new layer over a base entry of the same name.
- `--ovlfs-strip=1` "Strip[s] overlayfs metadata in the target image (e.g. whiteouts)": `lib/inode.c` removes whiteout entries after the base entries are loaded, and clears the opaque mark.
- **A whiteout and an entry of the same name in one layer.** `tarerofs_parse_tar` resolves each entry against the tree built so far from the same layer, so the result depends on order. With the entry first, the whiteout replaces it and the entry is lost. With the whiteout first, a later entry of that name replaces the whiteout (the branch for an existing dentry that is not a directory, or a non-directory entry, removes the old inode and allocates a new one). The name survives, but nothing of the whiteout survives either. For a directory entry that is the wrong result: when the base image's root is loaded, `erofs_rebuild_basedir_dirent_iter` (`lib/rebuild.c`) finds the layer's directory where the base has a directory of that name and re-points it at the base directory. `erofs_prepare_dir_inode` (`lib/inode.c`) then loads the base's children into it, because it is not opaque. The lower directory's children show through, where the specification deletes them.
- `.wh..wh..opq` only marks its directory opaque and its xattr (`lib/tar.c`, the `opq` branch). It removes no entry already in the tree, so same-layer children stay. An opaque directory skips the base load in `erofs_prepare_dir_inode`, so the lower children are hidden. A path whose parent directory has no entry of its own gets one from `erofs_rebuild_mkdir` (`lib/rebuild.c`).
- `--oci` builds from a registry reference itself, and the tar stream it builds from has `aufs` unset (`lib/remotes/oci.c`, `ocierofs_process_tar_stream`), so it leaves `.wh.` entries as ordinary files.
- erofs-utils 1.8.8 lists "Fix AUFS whiteout handling" for `tarerofs` (`ChangeLog`).
- `-U` sets the filesystem UUID.

Candidates weighed:

- **`mkfs.erofs`, one run per layer, with `--aufs --ovlfs-strip=1`** — chosen.
- **`mkfs.erofs --oci`.** It fetches from the registry inside the tool, which the converter has no network to do, and in v1.9.4 does not apply whiteouts.
- **All layers concatenated into one `mkfs.erofs --tar` run.** Every layer's entries land in one tree, and an opaque whiteout only marks its directory (`lib/tar.c`), so the lower layers' children already in the tree stay visible.
- **Per-layer EROFS images merged by rebuild mode.** Not checked against the source to the depth the chosen path was; it is the candidate if the chosen path's image size becomes the problem (Consequences).
- **The nydus family.** Nydus "implements a content-addressable file system on the RAFS format", served by its user-space daemon `nydusd` or, for uncompressed RAFS v6, mounted by in-kernel EROFS (nydus v2.4.5, `README.md`). What it adds over a plain EROFS is chunk sharing and lazy loading, which are stage v3; its whiteout handling was not checked, because stage v1 does not need it.

## Decision

The converter applies an image's layers in manifest order, one `mkfs.erofs` run each, from erofs-utils v1.9.4: the first with `--tar=f --aufs --ovlfs-strip=1`, each later one adding `--incremental=data`, writing to the output device. The filesystem UUID is the one mvmd chose (`-U`).

- **Compression.** A `tar+gzip` layer is filtered with `--ungzip`. A `tar+zstd` layer is decompressed by the converter before the stream reaches `mkfs.erofs`. Any other layer media type fails the pull.
- **Whiteouts.** A `.wh.<name>` entry deletes `<name>` from the layers below and is itself absent from the result; `.wh..wh..opq` hides every entry of its directory from the layers below. The result holds no whiteout and no opaque mark, so the guest's overlay sees a plain lower layer.
- **A whiteout whose name the same layer also holds is rewritten before `mkfs.erofs` sees it**, so the result is the specification's in every order. The layer's own `<name>` is kept, and the layers below lose theirs.
  - If the layer makes `<name>` a directory (an entry for it, or an entry under it), the converter replaces `.wh.<name>` with `<name>/.wh..wh..opq`. The directory then holds only this layer's children.
  - If the layer makes `<name>` anything else, the converter drops `.wh.<name>`. The layer's entry replaces whatever lay below.
  - A whiteout whose name the layer does not hold passes unchanged.
- **Two passes over each layer.** The host streams each verified layer to the converter twice. The first pass decompresses it and reads only the tar headers, to collect the layer's names. The second pass decompresses it again and feeds `mkfs.erofs` the rewritten stream. The converter keeps no copy of the layer, so its memory holds the set of names, not the layer. The cost is a second transfer over vsock and a second decompression per layer. A name set larger than the converter's memory bound fails the pull with that reason.
- **Pinned in the converter initrd.** The `mkfs.erofs` version is part of the converter build, and each MachineImage records the converter it came from ([ADR 0007](./0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md)).

## Consequences

- The result is a single EROFS with no lower-layer machinery, which is what the guest boot order mounts (`guest.md`).
- An incremental run appends: data of a file a later layer deletes or replaces stays in the image as dead space, so an image can be larger than its merged tree.
- A first code pull request carries conversion fixtures that exercise a whiteout, an opaque whiteout and a replaced directory. They also cover a whiteout and a same-named entry in one layer, in both orders and for both a file and a directory, each asserting the OCI layer specification's result. A change of `mkfs.erofs` version is checked against them.
- Moving to chunked EROFS, deduplication and lazy loading (stage v3) revisits the tool.

## Signs the decision was wrong

- A converted image shows a file a whiteout in a later layer deletes, or misses one the specification keeps.
- Dead space from incremental runs is a large share of the images clients pull.
- The second pass over each layer is a large share of conversion time.
- A later erofs-utils release changes the behaviour of `--aufs`, `--incremental` or `--ovlfs-strip` the fixtures check.
