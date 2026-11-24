# ADR 0072: A client reads a disk no Machine is running, or a disk snapshot, as a stream, and creates a disk from one

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A disk is node-local (ADR 0036) and a disk snapshot is a file beside the disks (ADR 0056). A client that moves a disk to another node (naru ADR 0109 item 8, naru #384) can reach the bytes of neither through the runtime API: Create disk takes at most 1 MiB of contents (ADR 0060), and no call returns a disk's bytes. What is open is how the bytes leave and enter a node, what makes a read of a live disk meaningful, and how a receiving node knows it holds what the sender read.

## Decision

**Read disk is a server-streaming call, and Create disk from a stream is a client-streaming call**, as Add boot set is (ADR 0063): mvmd opens no path a client names. Read disk names a disk or a disk snapshot, exactly one (invalid otherwise), and sends a header with the size and filesystem, then extents, then the SHA-256 in hex of the disk's whole logical content. An extent is an offset and bytes; a hole and a block of zeros are not sent, so a sparse disk is sent sparse. The digest covers the full size, a hole and a skipped block counting as zeros, so the digest does not depend on how the file is laid out.

**A read takes the disk as a Machine's snapshot of it does not.** A disk no Machine holds is held for the call, so no attach, delete, resize or snapshot takes it meanwhile. A disk a Created or Stopped Machine holds is read under that Machine's lock, so a start waits for the read. A disk a Running, Paused or Faulted Machine holds is precondition failed, because a read of a file a guest is writing has no digest that means anything. A snapshot is read under a shared hold of the catalog's lease on it (ADR 0067), so Delete disk snapshot is refused while a read runs. An unknown disk or snapshot is not found. A disk is read through a hold on the disk itself, and not by snapshotting it and reading the snapshot: Snapshot disk copies the whole disk, so a read through the snapshot path would double the disk's space for the length of the read, and a disk is moved because the node is short of room.

**Create disk from a stream takes the identifier, a request ID, the size and the filesystem as recorded at the source in its start message, then extents, then the SHA-256 in its final message.** The digest comes last on both sides, so a move is one pass over the source: the sender hashes while it reads, and the node hashes while it writes (naru ADR 0109 item 5.3). This replaces the digest in the start message that issue #827 asked for. The node writes the extents into a temporary file of the size, hashes what it writes with zeros for the gaps, and publishes the file only if the digest the stream ended with is the one it computed. A mismatch, an extent past the size or out of order, a second start message, a message after the digest, or a stream that ends without a digest is invalid, and nothing is left in `disks/`. The order check is what keeps a published file equal to its digest: an extent that begins before the end of the one before would overwrite bytes already hashed. The digest is recorded with the request ID as the disk's contents digest (ADR 0060). A retry streams again: the node does not take the digest at the start, so it has nothing to compare before the bytes are read. When the identifier exists, a request ID, size or filesystem that differs is a conflict before the stream is read; otherwise the stream is read and hashed, and the same digest is success and another is a conflict. A node does not format the disk it receives, whatever its filesystem: the bytes carry the filesystem. Zero blocks are not written, so the disk is sparse where the file system allows.

**A size is bounded before the lock, and hashing is cancellable.** A size above 16 TiB, the largest an ext4 filesystem of 4 KiB blocks holds, is invalid before the disk's lock is taken, since the digest covers the whole size and a gap of zeros is hashed. Hashing a gap checks the call's context, so a deadline or a client that left ends it.

**A peer that stalls does not hold the disk.** Each receive and each send of the two calls is bounded by an idle timeout, two minutes by default; when it passes, the call's context is cancelled, the call ends with failed, and the locks are released. The bound is on one message and not on the call, so a slow stream that moves is not cut.

**A node removes the temporary files an ended process left in `disks/` when it starts.** The temporary files are named `tmp-`, which no disk identifier may begin with.

**The digest is the only integrity check, and it is the client's to compare.** mvmd compares the received bytes with the digest the stream ended with; whether that digest is the source's is the caller's, who has Read disk's.

## Consequences

A client moves a disk or a snapshot between nodes through two calls it already speaks gRPC for, and the receiving node never holds a half-written disk. A read of a disk keeps it from a start, a delete and a resize for as long as the client takes to consume the stream, so a slow reader holds the disk; a client that stops reading for the idle timeout has the call ended and the disk freed. A retry of a failed Create disk from a stream sends the stream again, which costs a second transfer where a digest at the start would have cost none. A disk of a Running Machine is read from a snapshot of it, taken first. The two calls add messages to `v1alpha2`, so the version stays. Transport between hosts and the streaming of a Machine's memory snapshot are not decided here.

## Signs the decision was wrong

Clients read the disk of a Running Machine and want the crash-consistent bytes without taking a snapshot first. Clients hold a read open long enough that starts wait on it in practice. Disks whose zero blocks matter on the receiver, as allocated and not as zeros. A retry of a finished Create disk from a stream is common enough that sending the stream again is felt. A digest other than SHA-256, or one over the stored file and not the logical content, is wanted.
