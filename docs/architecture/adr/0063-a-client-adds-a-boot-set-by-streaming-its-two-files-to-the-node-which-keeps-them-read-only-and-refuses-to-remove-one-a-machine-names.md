# ADR 0063: A client adds a boot set by streaming its two files to the node, which keeps them read-only and refuses to remove one a Machine names

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

ADR 0058 made a boot set a name in the node's configuration, with a kernel file and an initrd file by path and SHA-256, and said "there is no upload by a client". A client that rolls kernel versions onto hosts it does not reinstall (aurora#811) needs to add a boot set to a running node, and to remove one, without editing the configuration or restarting mvmd (#746). What is open is how the files reach mvmd, where an added set is kept so that it survives a restart and its files cannot change unnoticed, and how a removal is kept from leaving a Machine whose record names a removed set.

## Decision

**The bytes are streamed over the API.** Add boot set is a client-streaming call: a first message with the name and both SHA-256 digests in hex, then the kernel's chunks, then the initrd's chunks. mvmd reads no path of the host on a client's word, so a client cannot make the daemon open a file the client could not itself read, and the call works for any client that reaches the socket. The rejected alternative, a host path that mvmd reads and copies, leaves mvmd opening a path it was given, and the file can change between the client's check and mvmd's read.

**mvmd hashes what it stores, and keeps it read-only in its own directory.** Each file is written to a staging directory while its SHA-256 is computed over the bytes written, and is compared with the declared digest only after the whole stream has arrived. A file that does not match is refused as invalid, naming it (`kernel` or `initrd`), and nothing is published. A set that matches is synced and published whole by one rename of its directory to `boot-sets/<name>` in mvmd's state directory, which holds the two files at mode 0444 and a record of both digests. The rename is the add: a crash leaves the set published or not, never half, and start-up deletes what staging holds. No database record is kept beside it, so there is no second place for the set to be half-present in. An added set survives a restart because the directory is its only state. Start checks both files against the recorded digests before the VMM is started, as it does for a configured set (ADR 0058), so a file changed after add fails the check as precondition failed, naming it, and the Driver's own pinned-file check at boot stands behind it.

**A name is one set.** The node's configured names and the added names are one namespace. An add of a name that is taken with both digests the same is success with no change, whether the name is configured or added, and with other digests is a conflict. A configured set cannot be removed through the API (precondition failed). Runtime info lists every set, configured and added, ordered by name, each with its digests and its origin, `configured` or `added`; no new list call is added.

**Remove is refused while a record names the set, and a remove that races a Create is refused as the same race of Remove image is.** The rule is the one `RemoveImage` applies (runtime-api.md, Remove image): Create takes the set's name in a keyed lock, shared, before it resolves the name and holds it until the Machine's record is set; Remove takes the same lock exclusively, only if nothing holds or waits on it, and refuses as precondition failed otherwise, naming the Create. With the lock held it scans the Machine records, in any state, for the name, refuses as precondition failed naming the first by identifier, and only then deletes the directory. So a Create that resolved the name has set its record before a Remove can scan, and a Create that starts after the Remove sees the name as not offered. A failed attempt's record holds no Machine and names nothing. Remove of a name the node does not hold is success.

**Nothing else is added.** There is no version beyond the name and the two digests, and no reclaiming beyond Remove: the node keeps an added set until a client removes it.

## Consequences

A client adds a kernel and initrd to a running node and creates OS-form Machines from them as from a configured set, on both Drivers. The state directory holds the added files, so a host's free space bounds what a client can add, and a client that adds sets without removing them fills it; reclaiming is the client's, as for images (ADR 0034). The add moves the whole files through the daemon, which is bounded by the stream's flow control and not by memory. A Machine whose record names a removed set cannot exist: the removal is refused first, so the Start-time "name no longer offered" refusal of ADR 0058 remains only for a configured name taken out of the configuration. This amends ADR 0058's "A boot set is named by the node" and its sentence "there is no upload by a client"; the rest of ADR 0058 stands.

## Signs the decision was wrong

Clients add files larger than a stream can carry in the time they allow. Clients want an added set to be shared between nodes by reference, which would mean a path or a URL. A node's state directory fills with added sets no client removes. A client needs to replace a set's files under its name, which would mean a version or a generation beyond the name and digests.
