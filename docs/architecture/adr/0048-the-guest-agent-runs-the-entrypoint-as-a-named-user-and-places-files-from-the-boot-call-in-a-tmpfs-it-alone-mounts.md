# ADR 0048: the guest agent runs the entrypoint as a named user and places files from the Boot call in a tmpfs it alone mounts

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The entrypoint ran as root with an image's files only. A client needs to run it as the image's user, and to hand it files that must not be in the image or on the root disk, among them Secret data. The agent resolves names against the booted root, so the answer exists only after the switch root. Rust's `CommandExt::groups` is unstable in the pinned toolchain, so the supplementary groups cannot be set through it.

## Decision

`RunConfig.user` is an OCI-form string resolved after the switch root against the booted root's `/etc/passwd` and `/etc/group`, and `RunConfig.files` lists files the agent writes into a tmpfs at `/run/mvmd/files` (mode 0700, root) and bind-mounts read-only onto their paths in the booted root. The boot gains the steps `place the files` and `apply the user`, in that order after the network, so a placed passwd or group serves the resolution. The entrypoint and `Exec` set the groups, gid and uid in a `pre_exec` closure, in that order, after the working directory is entered. The target is resolved within the booted root before anything is mounted, as the file calls resolve a path ([guest.md](../guest.md) "Paths follow links"): every link in the path is followed, an absolute link target is read from the booted root, and the reserved-directory check applies to the resolved path as well as the path sent. A resolved target that exists and is not a regular file is refused, naming the path sent; a dangling link whose parent exists gets its resolved file created; the resolved file is the one bound, and the link stays. Files are part of the description a repeat `Boot` is compared by. The agent never writes a file's content to a log, the console, an error or a status detail.

## Consequences

An image file at a placed path is shadowed and stays on the image. Content never reaches the root disk, but the empty target file (0644) and the parent directories (0755, root) the placing creates where the image has none are on the root disk's upper layer and persist across boots. Content lives in guest memory and is gone with the Machine. The limits (64 files, 1 MiB, reserved prefixes, clean absolute paths) are checked at the parse, so a client learns of a bad set before any step runs. Both fields are additive to v1alpha1, and an older client's empty values keep root and no files. The crate takes one `unsafe` block, for `pre_exec`, and the `nix` `user` feature. A bind mount is per path, so a directory is not placed as a tree.

## Signs the decision was wrong

Clients ask to place a directory tree or a file larger than the total limit. A placed file needs to be writable or to change while the Machine runs. `CommandExt::groups` stabilises and the `unsafe` block can go. A name must resolve before the files are placed.
