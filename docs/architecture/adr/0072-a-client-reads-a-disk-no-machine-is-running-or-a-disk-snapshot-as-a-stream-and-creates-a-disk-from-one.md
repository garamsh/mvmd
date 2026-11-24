# ADR 0072: A client reads a disk no Machine is running, or a disk snapshot, as a stream, and creates a disk from one

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A disk is node-local (ADR 0036) and a disk snapshot is a file beside the disks (ADR 0056). A client that moves a disk to another node (naru ADR 0109 item 8, naru #384) can reach the bytes of neither through the runtime API: Create disk takes at most 1 MiB of contents (ADR 0060), and no call returns a disk's bytes. What is open is how the bytes leave and enter a node, what makes a read of a live disk meaningful, and how a receiving node knows it holds what the sender read.

## Decision

**Read disk is a server-streaming call, and Create disk from a stream is a client-streaming call**, as Add boot set is (ADR 0063): mvmd opens no path a client names. Read disk names a disk or a disk snapshot, exactly one (invalid otherwise), and sends a header with the size and filesystem, then extents, then the SHA-256 in hex of the disk's whole logical content. An extent is an offset and bytes; a hole and a block of zeros are not sent, so a sparse disk is sent sparse. The digest covers the full size, a hole and a skipped block counting as zeros, so the digest does not depend on how the file is laid out.

**A read takes the disk as a Machine's snapshot of it does not.** A disk no Machine holds is held for the call, so no attach, delete, resize or snapshot takes it meanwhile. A disk a Created or Stopped Machine holds is read under that Machine's lock, so a start waits for the read. A disk a Running, Paused or Faulted Machine holds is precondition failed, because a read of a file a guest is writing has no digest that means anything. A snapshot is read under a shared hold of the catalog's lease on it (ADR 0067), so Delete disk snapshot is refused while a read runs. An unknown disk or snapshot is not found.

**Create disk from a stream takes the identifier, a request ID, the size, the filesystem as recorded at the source and the expected SHA-256, then extents.** The node writes the extents into a temporary file of the size, hashes what it writes with zeros for the gaps, and publishes the file only if the digest is the expected one. A mismatch, an extent past the size or out of order, or a stream that ends early is invalid, and nothing is left in `disks/`. The digest is recorded with the request ID as the disk's contents digest (ADR 0060), so the repeat of Create disk applies: the same identifier, request ID, size, filesystem and digest is success without the stream being read again, and any other is a conflict. A node does not format the disk it receives, whatever its filesystem: the bytes carry the filesystem. Zero blocks are not written, so the disk is sparse where the file system allows.

**The digest is the only integrity check, and it is the client's to compare.** mvmd compares the received bytes with the digest the caller gave; whether that digest is the source's is the caller's, who has Read disk's.

## Consequences

A client moves a disk or a snapshot between nodes through two calls it already speaks gRPC for, and the receiving node never holds a half-written disk. A read of a disk keeps it from a start, a delete and a resize for as long as the client takes to consume the stream, so a slow reader holds the disk; a client that stops reading ends the call and frees it. A disk of a Running Machine is read from a snapshot of it, taken first. The two calls add messages to `v1alpha2`, so the version stays. Transport between hosts and the streaming of a Machine's memory snapshot are not decided here.

## Signs the decision was wrong

Clients read the disk of a Running Machine and want the crash-consistent bytes without taking a snapshot first. Clients hold a read open long enough that starts wait on it in practice. Disks whose zero blocks matter on the receiver, as allocated and not as zeros. A digest other than SHA-256, or one over the stored file and not the logical content, is wanted.
