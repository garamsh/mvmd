# ADR 0058: An OS-form Machine boots a node-named kernel and initrd with a command line and disks attached at boot, and has no image or Guest Agent

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A Machine so far is an image: the node converts it to a root disk, boots a guest kernel with the Guest Agent as init, and the agent runs the entrypoint. A client that needs a Machine which is not a container image, a full operating system with its own init, had no way to ask for one (aurora#804, mvmd#660 items 1, 3 and 7, #661). What is open is how the request says so, where the kernel and initrd come from, how they are trusted, what the lifecycle does without an agent, and which calls still apply.

## Decision

**The OS form is a second form of Create Machine.** `CreateMachineRequest` gains `os_boot`, a boot set name and a kernel command line, beside the image form. A request takes one form: an OS-form request that sets any image-form field (`image_digest`, `root_disk_bytes`, `kernel`, `command`, `args`, `env`, `user`, `files`, `working_dir`, `probes`) is invalid, naming the field, and so is a disk with a `mount_path`, since no agent mounts it. Resources, the ceiling, disks and the network policy are as in the image form, and the network is unchanged. The change is additive, so the version stays `v1alpha2`.

**A boot set is named by the node.** The node's configuration lists boot sets by name, each a kernel file and an initrd file by absolute path and SHA-256, as ADR 0045 lists kernels. Runtime info lists the names with both digests. The Machine records the name at Create, a name the node does not offer is invalid, and there is no upload by a client. Start checks both files against their digests before the VMM is started and refuses a changed file as precondition failed, naming it. A name that is no longer offered when Start runs is precondition failed. The check is a port of the lifecycle (`BootFileChecker`), implemented in the daemon's wiring over the same pinned-file check the Drivers use, because the lifecycle has no filesystem.

**The disks are drives of the boot.** Each attached disk is passed to the Driver in attachment order with its read-only flag, and the Driver attaches them as drives at boot. Attach disk, Detach disk and a live Set resources are accepted on an OS-form Machine only while it is Created or Stopped, and are precondition failed while it is Running or Paused: nothing hot-plugs a disk the guest has no agent to find.

**There is no agent.** Start reaches Running when the Driver reports the VMM running, bounded by the node's boot timeout; no `Boot`, disk find or grow call is made. Stop sends no agent call and uses the Driver's own request to stop, then ends the VMM. Exec, Read exec output, Signal exec, the file API and Snapshot are precondition failed, naming the OS form. The console works as for any Machine. A run update on Start is invalid. A probe has nowhere to run, so it is refused at Create as an image-form field.

**No root disk, no image.** An OS-form Machine has neither: Inspect leaves `image_digest` and the root disk fields empty, and reports `os_boot`. Every reader of the Machine's root disk skips the empty value.

**The command line is the client's, after the Driver's.** Each Driver puts its own parameters first and the client's line follows; the Drivers state their parameters when they implement the form (#667). The line is at most 2048 bytes with no NUL and no newline.

**Until the Drivers boot it, each real Driver refuses.** A Driver given an initrd or a command line in `BootSpec` returns an error wrapping `ErrUnsupported` from Describe, which Start reports as unsupported with the Machine left Stopped.

## Consequences

A client boots an operating system on the node by a name it reads from runtime info, and the node's pinned files are the only kernels and initrds it can run. The daemon's lifecycle gains a second path with no guest step, which the in-memory Driver exercises. Features that need the agent are absent from this form, and a client that wants them uses the image form. The Firecracker and Cloud Hypervisor Drivers cannot yet start the form (#667), and its end-to-end test follows (#668).

## Signs the decision was wrong

Clients need to give a kernel or initrd of their own. Clients need a disk to be hot-plugged into a running OS. Clients need Exec or the file API on an OS-form Machine, which would mean an agent in the OS. Boot sets change often enough that a name in configuration is the wrong place for them.
