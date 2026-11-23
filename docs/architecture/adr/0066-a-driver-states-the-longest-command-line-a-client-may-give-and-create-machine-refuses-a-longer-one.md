# ADR 0066: a Driver states the longest command line a client may give, and Create Machine refuses a longer one

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

An OS-form Machine's kernel command line is the Driver's parameters, a space, then the client's line ([ADR 0059](./0059-each-driver-boots-an-os-form-machine-with-its-own-parameters-before-the-clients-command-line-and-refuses-a-line-the-kernel-would-cut.md)). Each Driver refuses at Describe a combined line over 2047 bytes. [ADR 0058](./0058-an-os-form-machine-boots-a-node-named-kernel-and-initrd-with-a-command-line-and-disks-attached-at-boot-and-has-no-image-or-guest-agent.md) bounded the client's line alone at 2048 bytes, so a line of up to 2048 bytes passed Create, was stored, and failed every Start (garamsh/mvmd#698). Two figures stood for one limit, and neither could follow the Driver's parameters.

## Decision

The Driver states the longest client line it can boot, and Create Machine refuses a longer one.

- **The figure.** `DriverLimits` carries `MaxCommandLine`, the bytes a client's line may take: the 2047 a guest kernel takes less the length of the Driver's parameters for an OS-form boot and less the one space between them. Each Driver computes it from the same parameters it gives the guest, so Describe and the figure cannot disagree.
- **The refusal.** Create Machine with an OS form refuses, as invalid and naming `os_boot.kernel_cmdline`, a line longer than the figure of the node's Driver. A line of exactly that length is accepted and boots. A Driver that states no figure is held to the 2047 a guest kernel takes.
- **What it replaces.** The bound of 2048 bytes in ADR 0058 is replaced by this figure. A NUL or a newline is refused as before.
- **What stays.** Describe still refuses a combined line over 2047 bytes, as the check for a boot the figure did not cover, such as a Machine stored before this change.
- **What is not decided here.** The figure is not reported in the runtime API's driver info, and no field is added to the API.

## Consequences

- A client learns of a line that no Driver could boot at Create, and no stored Machine fails every Start for its length.
- The figure differs by Driver and by architecture, so a client cannot know it before it creates.
- A Machine stored with a line the earlier bound accepted still fails at Start, with the Driver's refusal.

## Signs the decision was wrong

- A client needs the figure before it creates, and takes it by trial.
- A Driver's parameters depend on the Machine, so one figure per Driver no longer holds.
