# ADR 0017: The converter serves a gRPC contract it owns and carries mkfs.erofs in its initrd

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#65, split from #33. [ADR 0005](./0005-untrusted-images-are-converted-in-a-converter-microvm.md) decides what crosses into the converter microVM and what comes out of it, and [ADR 0006](./0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md) how it applies layers. Neither decides the encoding of the conversation, how `mkfs.erofs` gets into an initrd that has no root filesystem, or how a failure is reported. [ADR 0014](./0014-the-guest-agent-serves-a-grpc-contract-it-owns-over-vsock.md) settled the first for the Guest Agent, and `imageconv/` is the same kind of unit: Rust, a static musl init, served over vsock.

What was found building it:

- **erofs-utils v1.9.4 ships no `configure`.** The tag holds `autogen.sh` and `configure.ac` (`docs/INSTALL.md` runs `./autogen.sh`, `./configure`, `make`), and no `CMakeLists.txt`. The tag's tarball on GitHub is a `git archive` of it, and GitHub's releases API returned no release assets for the tag.
- **The development host has no `musl-gcc`, `autoconf`, `automake` or `libtool`.** It has `gcc`, `clang`, `make`, `perl` and `pkg-config`. CI's `ubuntu-24.04` was not checked for any of them before this ADR.
- **`mkfs.erofs` spools file data.** `lib/diskbuf.c` (`erofs_diskbuf_init`) keeps the data of a tar layer's regular files in one temporary file. When the output is a regular file it extends that file and uses it; when the output is a block device, which `ftruncate` refuses, it creates the file with `mkstemp` in `$TMPDIR` or `/tmp`. Read from source; a run against a block device was not made.
- **ADR 0006 asks for two things that meet on gzip.** A `tar+gzip` layer is "filtered with `--ungzip`", and the second pass "feeds `mkfs.erofs` the rewritten stream". A rewrite reads plain tar, so a gzip layer that needs one cannot reach the tool compressed.
- **`mkfs.erofs` ignores a tar entry whose type flag is NUL** (`lib/tar.c`, `tarerofs_parse_tar`: no `case` for it), takes the size field as the data length for every type, and failed with `invalid tar @ 0` on a header whose owner fields were empty, as the `tar` crate leaves them.

Candidates weighed, for how `mkfs.erofs` enters the initrd:

- **Built from pinned source tarballs by a checked-in script, with the host's `gcc`** — chosen. musl, kernel headers, zlib and GNU autotools are built first from pinned tarballs, so no header or library of the host reaches the result.
- **A pinned container image with the toolchain installed by `apk`.** The image has a digest, the packages installed into it do not: an Alpine package index moves within a release. It also needs a container runtime on every machine that builds, where the repository's checks run `make` on the host.
- **A distribution's `mkfs.erofs`.** Its version is the distribution's, it is not static, and it is not pinned by this repository.
- **Compiling the sources directly with a hand-written `config.h`.** It skips autotools, and becomes a second build system for erofs-utils that every version bump has to be re-derived against.
- **A pinned `gcc`.** Building one is the longest step by far, and every other input is already pinned.

For the converter's contract, the alternatives ADR 0014 weighed hold unchanged (a hand-framed protocol; the contract in the daemon's tree; committing generated Rust). Two more were weighed here:

- **One call per layer, the converter keeping the image between calls.** The report would then belong to a call that is not the last, and an out-of-order call would have to be detected from state.
- **Reporting a failed conversion as a gRPC status.** The step that failed is data the daemon branches on, and a status code cannot carry a step without a second encoding beside it.

## Decision

- **Wire format.** The converter serves a gRPC service, `mvmd.convert.v1alpha1.ConvertService`, on vsock port 1025, with one RPC, `Convert`, which takes a client stream. The package carries the version, by the rule of the runtime API (`runtime-api.md` §Scope and version, "Rule for changing the version").
- **The contract's home.** `imageconv/proto/mvmd/convert/v1alpha1/`. The daemon reaches the converter's tree only through that file, as it does the Guest Agent's ([ADR 0014](./0014-the-guest-agent-serves-a-grpc-contract-it-owns-over-vsock.md)); `buf lint` covers it, and the Rust code is generated at build time and not committed.
- **The request stream.**
  - A `Start`, first and once: the filesystem UUID, the media type of every layer in manifest order, and the most bytes of entry names the converter holds for one layer.
  - Then, for each layer in order, two passes, each a `Pass` (the layer's index and its kind, names or content) followed by the layer's bytes as `Data` messages of at most 1 MiB, exactly as the registry served the blob. The names pass is the first read of ADR 0006 and the content pass the second.
- **Media types arrive in `Start`.** The converter refuses the conversion before reading any layer when one is not `tar`, `tar+gzip` or `tar+zstd`, distributable or not, so no layer is converted for nothing. A UUID that is not in the 8-4-4-4-12 form, and an image with no layers, are refused the same way. Nothing but a parsed UUID reaches the tool's command line.
- **The answer.** One `ConvertResponse`, after the last pass or as soon as the conversion has failed: either `Converted` with the EROFS length in bytes, or `Failure` with a step and a message. The converter serves one conversion: it refuses a second `Convert` call with `ALREADY_EXISTS`, and powers the VM off once the answer has been sent or five seconds after.
- **Failure reporting.**
  - An expected failure is the `Failure` message, with one step: the start was unusable, a layer type was refused, the messages broke the call's order, a layer could not be read in its names pass, a layer's names exceeded the limit, a content pass or `mkfs.erofs` failed (the message carries the end of the tool's standard error), or the output was unusable. The output is unusable when no EROFS superblock is on it, when its UUID is not the one asked for, or when its block size is out of range.
  - A gRPC status means the call itself broke: a transport error, a second call, a panic on the conversion's thread.
  - A failure of the converter outside any conversion (it cannot prepare the VM, find its output device or bind vsock) is one console line, `mvmd-imageconv: failed to <context>: <error>`, and the VM powers off, as the Guest Agent's init does. A panic does the same. After any failure the output's content is undefined, and the host discards the staging directory ([ADR 0007](./0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md)).
- **The length.** The converter reads it from the EROFS superblock on the output, the block count times the block size, and not from the file's length, since a block device has the size of the whole device.
- **The tool's arguments.** Those of ADR 0006, with `--quiet`, and with `-T0 --mkfs-time`, which fixes the build time and, by `mkfs.erofs --help`, applies the timestamp to the build time only. The same layers and UUID then give the same image, byte for byte; the fixtures build one image twice and compare.
- **Gzip.** A `tar+gzip` layer for which the names pass found nothing to rewrite reaches `mkfs.erofs` as it came, with `--ungzip`. One that needs a rewrite is decompressed by the converter, rewritten, and reaches the tool as plain tar. Every names pass decompresses, since it reads tar headers. A `tar+zstd` layer is always decompressed by the converter, and every frame of it is read.
- **The output device.** The one virtio block device in `/sys/block`; none or several fails the VM's start, after up to ten seconds for the kernel to register it.
- **Scratch space.** `/tmp` is a tmpfs. Where the output is a block device, `mkfs.erofs` spools a layer's file data there, which is memory: tmpfs takes half of the VM's memory by default, so the node's configured memory for a conversion has to exceed twice the largest layer's file data. Memory exhausted fails the pull as ADR 0005 says.
- **How `mkfs.erofs` enters the initrd.**
  - `imageconv/mkfs-erofs/build.sh` builds `mkfs.erofs` and the `fsck.erofs` the fixtures read its output with, statically linked, from pinned inputs: erofs-utils v1.9.4 named by the commit its tag points to, musl 1.2.6, zlib 1.4, kernel headers from sabotage-linux/kernel-headers v6.12.77, and the GNU m4 1.4.21, autoconf 2.73, automake 1.20 and libtool 2.6.2 that erofs-utils's `autogen.sh` needs. Each is a tarball whose SHA-256 the script checks before unpacking, and fails on a mismatch ([ci.md](../../convention/ci.md) §Verify a pinned dependency before adopting it).
  - The tools are built with lz4, lzma, zstd, libuuid and every remote and FUSE feature off, and `pkg-config` pointed at the build's own zlib only, so the host's libraries cannot be linked.
  - The script builds the tools twice, in different directories with their paths remapped, and fails unless the two are identical. `make build` runs it on every build, and `make test` runs it when its pins changed.
  - The initrd is `/init`, the converter, and `/mkfs.erofs`, in a newc archive that is a function of those two files' bytes. `make build` builds the converter twice, as it builds the Guest Agent, and fails unless the two archives are identical.
- **What moves a pin.** Nothing does automatically: Dependabot reads no tarball. The owner of `imageconv/` reviews the upstream releases of each input whenever erofs-utils releases, and moves a pin in a pull request that reruns the fixtures of ADR 0006, which is the check that ADR's Consequences ask of a change of version. Each pull request that moves one records the advisory source it consulted for the new version.
- **What was checked for advisories on 2026-11-09.** The NVD CVE API, queried by CPE name for erofs-utils 1.9.4, zlib 1.4 and musl 1.2.6, returned no result for any. A keyword search for erofs-utils returned CVE-2023-33551 and CVE-2023-33552, both against v1.6. Searches by keyword for recent musl and zlib entries returned no body, and so established nothing. No advisory source was consulted for the GNU build tools, whose output is not in the result, or for the kernel headers.

## Consequences

- The daemon's Converter channel adapter is written against the `.proto` and needs no VM to be tested, since the server runs over a Unix socket.
- Every `make build` and the first `make test` fetch about ten megabytes and build a toolchain; later runs of `make test` find the tools built.
- The tools are built with the host's `gcc`, which is not pinned. Two builds on one host give one digest, and the same sources on a host with another `gcc` may give another. Two builds in different directories of the development host gave the same digest, so the paths of the build are not in it.
- The host needs `gcc`, `make`, `perl`, `curl`, `tar`, `xz`, `gzip`, `sha256sum`, binutils and `pkg-config` with its `pkg.m4` in `/usr/share/aclocal`; the script says which is missing where it can tell.
- The converter's memory has to be sized from the layers it is given (Scratch space), which node configuration states.
- A layer is read by the converter's own tar reader before the tool sees it, in both passes. A tar stream that the reader and `mkfs.erofs` read differently could hide a whiteout from the first; the reader follows `lib/tar.c` for entry types, paths and sizes, and the worst result is a wrong EROFS for that one image, as ADR 0005 states.
- The opaque marker removal and the absence of whiteouts are asserted through the tree `fsck.erofs --extract` makes, as an unprivileged user, so an overlay xattr left on a directory is not seen by the fixtures.

## Signs the decision was wrong

- The second pass over each layer, or the converter's tar reader, is a large share of conversion time, or a hostile archive makes the reader and the tool disagree about an entry.
- Pulls fail for lack of memory in `/tmp` at layer sizes clients pull, so the scratch space has to move to a disk the VM is given.
- The gzip layers that reach the tool as they came are too few to pay for the second path, and decompressing every gzip layer in the converter would be simpler.
- A pin cannot be moved without rewriting `build.sh`, or two hosts building one source give images that differ in ways the fixtures catch.
- A change of host `gcc` changes the digest of the initrd in a way an operator has to chase, so the compiler needs pinning.
- The daemon needs to tell the failures of one step apart further than the steps do.
