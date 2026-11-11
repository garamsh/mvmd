# mvmd

A node-local microVM runtime daemon: the kind of component containerd is to Kubernetes, for microVMs. It runs Machines on one node through a VM-native runtime API that it defines and versions, and it does not know gaon, the orchestration project that is its client.

The repository holds the daemon, the Guest Agent, the converter, the runtime API library and the end-to-end tests, with the conventions and the architecture they follow.

## What it does

- Runs Machines: create, start, stop, pause, resume, snapshot and restore, disk attach and detach, resource resize, inspect, list, events, exec, file API and console.
- Pulls images, converts them to MachineImage and keeps the node image cache and the snapshot store.
- Drives VMMs through a VMM-neutral Driver contract; Firecracker and Cloud Hypervisor first.
- Runs a Guest Agent inside each Machine.
- Stops every Machine when its client stops renewing a local fencing deadline.

It contains no container runtime, and it does not decide when a Machine runs.

## Where to read

- `docs/architecture/README.md` — the index of the architecture documents and decisions
- `docs/architecture/structure.md` — the shape, the repository layout and the boundaries
- `docs/convention/README.md` — the conventions every change follows
- `AGENTS.md` — the contribution contract: who may change what

## Structure

- `AGENTS.md` — the contribution contract
- `docs/convention/` — conventions for code, reviews and documentation
- `docs/architecture/` — responsibility documents (current truth) and ADRs (append-only decision log)
- `docs/procedure/` — procedures for this repository's recurring acts
- `.github/` — PR and issue templates, and CODEOWNERS

The code folders and what each holds are listed in `docs/architecture/structure.md` §Repository layout.
