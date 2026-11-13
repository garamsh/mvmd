# ADR 0059: Each Driver boots an OS-form Machine with its own parameters before the client's command line, and refuses a line the kernel would cut

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

ADR 0058 gave an OS-form Machine a boot set's kernel and initrd, a client's kernel command line, and disks attached at boot, and had each real Driver refuse the form as unsupported until the Drivers could boot it. The open points are what each VMM is given, which parameters the Driver puts on the line, and what happens to a line the kernel would truncate: the client's line may be up to 2048 bytes, and a guest kernel takes at most 2047 (x86_64's `COMMAND_LINE_SIZE` of 2048 counts the terminating NUL).

## Decision

**Each Driver boots the form.** It takes the boot set's initrd from `BootSpec.Initrd`, with its digest, in place of the initrd it is configured with, and passes the disks of `BootSpec.Devices` as drives in order with their read-only flags. An image-form boot is unchanged. This amends the paragraph of ADR 0058 that has each real Driver refuse; nothing else of ADR 0058 changes.

**The Driver's parameters come first, and the client's line follows.** The Driver puts the parameters its serial console and its stop request need: `console=` for the VMM's serial device, `reboot=k panic=1`, and on x86_64 the i8042 probe parameters. It adds no `mvmd.end`, which only the Guest Agent reads, and no memory hot-plug policy, since no resize reaches an agent-less Machine. A parameter the client repeats overrides the Driver's where the kernel takes the last. `driver.md` states the parameters of each Driver.

**A line over what the kernel takes is refused.** Describe refuses a boot whose combined line, the Driver's parameters and the client's, is over 2047 bytes, as an ordinary failure of the boot and not as unsupported. The VMMs do not refuse it themselves, and a kernel truncates it silently.

**No disk is a boot.** An OS-form Machine may hold no disk, so Describe accepts an empty `Devices` for it, and Cloud Hypervisor is given no `--disk` option, which takes a value.

## Consequences

- A client's line within the API's bound can still be refused at Start, by the length of the Driver's parameters. Lowering the API's bound to what leaves room for the longest Driver's parameters would refuse it at Create; that bound is `maxCommandLine` in `daemon/internal/machine/bootsets.go`.
- The stop request of each Driver reaches the guest as before, and what the guest must do with it is a requirement of the guest kernel and its init, stated in `driver.md`. An OS-form guest that does not act on it is ended by the lifecycle after the grace, as ADR 0018 says.
- A new Driver states its parameters in `driver.md` and takes the same refusal.

## Signs the decision was wrong

- Clients' command lines are regularly refused for length at Start.
- A guest needs a parameter before the client's line that the Driver cannot know, such as a console of its own.
- The Driver's parameters prove to be the client's to choose, because two clients want two different consoles or stop methods.
