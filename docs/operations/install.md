# Installing mvmd on a host

The steps that put mvmd on a host and start it: the host's needs, what to place where, the configuration, the unit, and the first start.

Checked as `README.md` states. A step marked "read" rests on reading the code at that commit and was not run.

## Contents
- The host's needs
- Place the files
- Write the configuration
- Install the unit
- Offer boot sets
- Start it and check it

## The host's needs

mvmd refuses to start where one of these is missing, and says which.

| Need | Why | Source |
|---|---|---|
| Linux with cgroup v2 mounted at `/sys/fs/cgroup`, and Landlock ABI 6 | The supervisor confines each VMM with them; mvmd refuses to start on a kernel without Landlock ABI 6 | `docs/architecture/runtime.md` §VMM confinement; `daemon/internal/supervisor/landlock.go:65` (read) |
| `/dev/kvm` and `/dev/net/tun` | A VMM needs the first, and a Machine's tap needs the second | `daemon/internal/firecracker/describe.go` `process` (read) |
| `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE` and `CAP_NET_ADMIN` for mvmd | The supervisor's PID namespace, confirming that a namespace has ended, and the taps and the nftables table | `docs/architecture/runtime.md` §Fencing, §Network; `daemon/internal/supervision/supervision.go:77` (read) |
| `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` at 1, for each family the node serves | mvmd writes no sysctl and refuses to start on a served family that reads otherwise | `docs/architecture/runtime.md` §Network (read) |
| A state directory on ext4 or f2fs with the `verity` feature, or on btrfs | The image cache is guarded by fs-verity, and mvmd refuses to start where it cannot enable it | `docs/architecture/image.md` §Layout (read) |
| `disks/`, `snapshots/` and `disk-snapshots/` of the state directory on one file system, with the Machines' run directories | A snapshot is taken in by rename and a disk snapshot by clone | `docs/architecture/runtime.md` §State and stability (read) |
| `mkfs.ext4` | A root disk is formatted with it; mvmd runs it with `-V` at start | `daemon/internal/app/config.go` `DiskConfig` (read) |
| `nft`'s kernel side (nf_tables) | mvmd owns one table, `mvmd`, and changes it by transactions | `docs/architecture/runtime.md` §Network (read) |

mvmd and its supervisors run as root. Whether a narrower set of capabilities than root's would do has not been tried.

## Place the files

Build the two binaries from `daemon/` with the Go toolchain `go.work` names, or take them from a release. Run:

```
cd daemon
go build -o /opt/mvmd/mvmd ./cmd/mvmd
go build -o /opt/mvmd/mvmd-supervisor ./cmd/mvmd-supervisor
```

Place beside them, each at an absolute path of its own, the files the configuration pins: the VMM binary (Firecracker or Cloud Hypervisor), the guest kernel and initrd, the converter's kernel and initrd, and the kernel and initrd of each boot set. Take each file's digest with `sha256sum`; the configuration names it, mvmd checks it at start, and the supervisor checks it before every boot.

The state directory is created if it is absent, mode 0700 (`daemon/internal/boltstore/boltstore.go:36`). The socket's directory must exist.

## Write the configuration

The configuration is one JSON file, `mvmd -config <file>`. A field mvmd does not know is an error, so a misspelling is not ignored (`daemon/internal/app/config.go` `ParseConfig`, run). This example is for Firecracker, with its digests made up; mvmd's parser accepted it as written:

```json
{
  "socket": {"path": "/run/mvmd/mvmd.sock", "mode": "0660", "group": "mvmd-clients"},
  "state_dir": "/var/lib/mvmd",
  "driver": "firecracker",
  "vmm_uid_range": {"first": 100000, "count": 4096},
  "firecracker": {
    "binary": "/opt/mvmd/firecracker",
    "binary_sha256": "1111111111111111111111111111111111111111111111111111111111111111",
    "kernels": {"default": {"path": "/opt/mvmd/vmlinux", "sha256": "2222222222222222222222222222222222222222222222222222222222222222"}},
    "default_kernel": "default",
    "initrd": "/opt/mvmd/guest-agent-initrd.cpio",
    "initrd_sha256": "3333333333333333333333333333333333333333333333333333333333333333",
    "boot_sets": {
      "os": {
        "kernel": {"path": "/opt/mvmd/os-vmlinux", "sha256": "4444444444444444444444444444444444444444444444444444444444444444"},
        "initrd": {"path": "/opt/mvmd/os-initrd.cpio", "sha256": "5555555555555555555555555555555555555555555555555555555555555555"}
      }
    }
  },
  "conversion": {
    "kernel": "/opt/mvmd/converter-vmlinux",
    "kernel_sha256": "6666666666666666666666666666666666666666666666666666666666666666",
    "initrd": "/opt/mvmd/converter-initrd.cpio",
    "initrd_sha256": "7777777777777777777777777777777777777777777777777777777777777777",
    "converter": "mvmd-converter-1",
    "vcpus": 2,
    "memory_bytes": 2147483648,
    "output_bytes": 8589934592,
    "name_set_bytes": 67108864,
    "limit": "10m"
  },
  "disk": {"mkfs": "/usr/sbin/mkfs.ext4"},
  "supervision": {
    "binary": "/opt/mvmd/mvmd-supervisor",
    "cgroup": "/sys/fs/cgroup/mvmd-supervisors",
    "vmm_cgroup": "/sys/fs/cgroup/mvmd-vmm"
  },
  "network": {
    "ipv4_prefix": "10.77.0.0/24",
    "ipv4_gateway": "10.77.0.1",
    "resolvers": ["10.77.0.1"]
  }
}
```

What each section decides:

| Section | Notes |
|---|---|
| `socket` | The socket's file permissions are the only authentication, so `mode` and `group` name who may call mvmd. The group must exist. The path is bound as written, and a path over 107 bytes fails to bind with `invalid argument` (run). |
| `state_dir` | Absolute. Holds `mvmd.db` and the directories in [README.md](./README.md) §What mvmd keeps. |
| `driver` | `firecracker`, `cloud-hypervisor` or `memory`. A real Driver needs the `supervision`, `conversion`, `disk` and `network` sections, and the `memory` Driver refuses the `disk`, `conversion` and `registry` sections; the `memory` Driver runs no VMM and is for development (`daemon/internal/app/config.go` `check`, read). |
| `vmm_uid_range` | The UIDs and GIDs mvmd gives to Machines and to converters, one each. The first is not zero, and the count bounds the Machines the node holds. Choose a range no account of the host uses. |
| `firecracker` or `cloud_hypervisor` | The pinned binary, kernels by name, the default kernel, the guest initrd, and boot sets by name. At least one kernel; the default is one of them. |
| `conversion` | The converter's kernel and initrd, its size and its time limit. There is no default: a converter too small for an image fails the pull. |
| `supervision` | `binary` is the supervisor. `cgroup` must exist when it is set, and puts each supervisor outside mvmd's cgroup, which the unit below relies on. `vmm_cgroup` is made by mvmd. |
| `network` | The prefixes the node serves (IPv4, IPv6 or both), the gateway, and the resolvers. A real Driver requires it. |
| `registry` | Optional: a credentials file, credential helpers and a CA bundle. They are read at each pull, so a change reaches the next pull without a restart (`docs/architecture/runtime.md` §Registry access). |
| `boot_timeout`, `call_timeout`, `pull_timeout` | Optional Go durations. |

A change to the configuration takes a restart. See [upgrade.md](./upgrade.md).

## Install the unit

```
[Unit]
Description=mvmd node-local microVM runtime
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
ExecStartPre=/usr/bin/mkdir -p /sys/fs/cgroup/mvmd-supervisors
ExecStart=/opt/mvmd/mvmd -config /etc/mvmd/mvmd.json
Restart=on-failure
RuntimeDirectory=mvmd
StateDirectory=mvmd
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

`systemd-analyze verify` accepted this unit with `ExecStart` naming a binary that exists (run). What the unit relies on:

- **The supervisors must not be in the unit's cgroup.** systemd's default `KillMode=control-group` kills every process left in the unit's cgroup when the unit stops, and a deployment that leaves the supervisors there loses every Machine on each stop of mvmd (`docs/architecture/adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md` §Consequences). `supervision.cgroup` names a cgroup outside it, and mvmd opens that directory for each supervisor it starts, so it must exist before mvmd starts: that is the `ExecStartPre` (`daemon/internal/supervision/supervision.go:53`, read). `/sys/fs/cgroup` is not persistent, so the directory is made at each start.
- **`Restart=on-failure`** starts mvmd again after a crash. The Machines keep running meanwhile ([recovery.md](./recovery.md)).
- **`StateDirectory=mvmd`** is `/var/lib/mvmd`, the `state_dir` of the example. If it is on another file system than the unit's root, the file system must meet the needs above.

## Offer boot sets

An OS-form Machine boots a boot set by name. A set comes from one of two places:

- **The configuration's `boot_sets`**, as above. A set is a kernel and an initrd by path and digest, and a change needs a restart. The runtime API cannot remove one.
- **The runtime API's Add boot set**, which streams the two files to the node, which keeps them read-only in `boot-sets/` of the state directory. It needs no restart, and Remove boot set refuses a set a Machine's record names (`docs/architecture/adr/0063-a-client-adds-a-boot-set-by-streaming-its-two-files-to-the-node-which-keeps-them-read-only-and-refuses-to-remove-one-a-machine-names.md`; read).

A name a configuration holds is not replaced by an added set: Add boot set under that name with the same digests succeeds and changes nothing, and with other digests is a conflict (`docs/architecture/runtime-api.md` §Boot sets). Start checks both files' digests against the ones recorded, so a file replaced on the host fails the start and not the boot.

## Start it and check it

```
systemctl daemon-reload
systemctl enable --now mvmd
```

(read) mvmd does these before it serves a call, in this order: ends the converter VMs a previous mvmd left, sweeps the image cache, restores the Machines from the state directory and reconciles each against the live supervisors, then reconciles the fencing deadline, and only then opens the socket (`daemon/internal/app/app.go:267`). So the socket's existence is the readiness: wait for it.

`mvmd -check -config <file>` checks the configuration, the digest of each pinned file, the Landlock ABI and `mkfs.ext4` without starting anything (see [upgrade.md](./upgrade.md) §Before you stop mvmd for what it leaves out). A refused configuration or host ends the process with exit code 1 and one line on standard error, `mvmd: <reason>`, which the journal holds; a missing `-config` is exit code 2 (`daemon/internal/app/app.go:338`, run on the `memory` Driver). On the `memory` Driver, `SIGTERM` ended the process with exit code 0 and removed the socket (run).

To confirm what the node offers, ask it for runtime info with the Go client of `lib/runtimeapi/`: it lists the Drivers, the kernels, the boot sets with their origins, the capacity and the prefixes. There is no command line client yet ([README.md](./README.md) §What an operator cannot do today).

A second mvmd started on the same state directory while the first runs fails after one second with `open .../mvmd.db: timeout` (run): one process holds the database.
