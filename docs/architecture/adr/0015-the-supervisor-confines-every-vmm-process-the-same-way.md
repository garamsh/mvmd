# ADR 0015: The supervisor confines every VMM process the same way

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#23 and decided in review of the pull request that adds this ADR.

The guest and the image are untrusted (garamsh/discussions#2 §5, security, discussioncomment-19288321), so a guest that escapes its VM lands in the VMM process, and what that process can reach on the host is what the escape reaches. Every VMM process, a converter's included, runs under its own `mvmd-supervisor`, the init of a PID namespace that starts it ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)), and fencing was made independent of which VMM runs. The network rules confine what a guest sends through its tap; the VMM process itself was left in the host's network namespace, file system and UID ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md), Consequences). No container runtime is used ([ADR 0001](./0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)); ADR 0003 reads that as not reaching a kernel namespace used for one process's lifetime, and this ADR reads it the same way for confinement.

What the two VMMs bring, at Firecracker v1.17.0 and Cloud Hypervisor v54.0:

- **Firecracker's jailer** "is meant to address the security needs of Firecracker only and is not intended to work with other binaries". It copies the binary into a chroot, enters a new mount namespace and `pivot_root`s into it, creates `/dev/kvm` and `/dev/net/tun` with `mknod` and gives them to the VM's UID, optionally joins a network namespace and creates a PID namespace, writes cgroup files, sets rlimits, drops to the given UID and GID and execs. It runs as root, and "All inputs to the jailer are considered trusted" (`docs/jailer.md`). On aarch64 it also copies `/sys/devices/system/cpu/cpu0/cache` and `midr_el1` into the jail (`src/jailer/src/env.rs`, `copy_cache_info`), and it creates `/dev/urandom` for MMDS version 2 and `/dev/userfaultfd` when the host has one (same file).
- **Firecracker's seccomp filters** are on by default, "loaded in the Firecracker process, on a per-thread basis": the VMM thread "right before executing guest code on the VCPU threads", the API thread before its server starts, and the vCPU threads before guest code (`docs/seccomp.md`). In `resources/seccomp/x86_64-unknown-linux-musl.json` every thread's default action is `trap`, `socket` is allowed only for `AF_UNIX` with `SOCK_STREAM | SOCK_CLOEXEC`, and no thread may call `clone`, `clone3`, `unshare`, `setns` or `mount`; the VMM thread may call `io_uring_setup`.
- **Firecracker opens its tap by name.** `Tap::open_named` opens `/dev/net/tun` and attaches with `TUNSETIFF` (`src/vmm/src/devices/virtio/net/tap.rs`); the API takes no descriptor for a network device. Its API socket is a path, `--api-sock`, default `/run/firecracker.socket` (`src/firecracker/src/main.rs`), and its vsock device is a Unix socket at `uds_path` (`docs/vsock.md`).
- **Cloud Hypervisor's seccomp filters** are on by default and differ per thread, the main thread applying its own "after starting the worker threads" (`docs/seccomp.md`). The VMM thread's filter allows `socket` for `AF_UNIX`, `AF_INET`, `AF_INET6` and `AF_NETLINK`, and `clone` and `clone3` unconditionally (`vmm/src/seccomp_filters.rs`). Its one direct `clone3` caller falls back to `fork` on `ENOSYS` (`vmm/src/sigwinch_listener.rs`).
- **Cloud Hypervisor's Landlock** is opt-in (`--landlock`), applied at `vm_create` (`docs/landlock.md`), at Landlock ABI 3 and for file system rights only (`vmm/src/landlock.rs`). Its threat model says the sandbox "does not prevent access to AF\_UNIX sockets. This can be blocked via namespaces or by other kernel-enforced access controls" (`docs/threat-model.md`, Sandboxing).
- **Cloud Hypervisor's devices.** It opens `/dev/kvm` through kvm-ioctls 0.25.1 (`Kvm::new`, `src/ioctls/system.rs` of that crate); it takes a tap by descriptor (`--net fd=`, ADR 0011), its API socket as `path=` or `fd=` (`cloud-hypervisor/src/main.rs`), and its vsock device as a Unix socket path (`vmm/src/config.rs`, `VsockConfig::SYNTAX`). It opens no `/dev/vhost-vsock` (no occurrence in the tree). On aarch64 it reads `/sys/devices/system/cpu/cpu0/cache` (`arch/src/aarch64/cache.rs`).

The kernel behaviour the decision rests on, from Linux v6.12 unless another source is named; man pages are Linux man-pages 6.9.1:

- **Tap ownership.** A persistent tap is attached without `CAP_NET_ADMIN` only by its owner UID or group (`drivers/net/tun.c`, `tun_not_capable`). The tap is looked up by name in the network namespace of the process that opened `/dev/net/tun`, not of the one calling `TUNSETIFF` (same file, `tun_chr_open` and `__tun_chr_ioctl`).
- **Network namespaces** isolate network devices, the IP stacks, routing, firewall rules, ports, and "the UNIX domain abstract socket namespace" (`network_namespaces(7)`). A pathname Unix socket is found by its inode, with no network namespace check (`net/unix/af_unix.c`, `unix_find_bsd`; `unix_find_abstract` takes the namespace).
- **Mount namespaces and bind mounts.** `unshare(CLONE_NEWNS)` gives the caller a private copy of its mount namespace and needs `CAP_SYS_ADMIN` (`unshare(2)`); `pivot_root` "changes the root mount in the mount namespace of the calling process" (`pivot_root(2)`). A bind mount "makes a file or a directory subtree visible at another point" (`mount(2)`, `MS_BIND`).
- **User namespaces.** Unprivileged processes can create them, and the creator "starts out with a complete set of capabilities in the new user namespace" (`user_namespaces(7)`).
- **Capabilities.** When a thread's real, effective and saved UIDs all become nonzero from a state where one was 0, its permitted, effective and ambient sets are cleared; at `execve` the new permitted set is `(P(inheritable) & F(inheritable)) | (F(permitted) & P(bounding)) | P'(ambient)` (`capabilities(7)`). Dropping from the bounding set needs `CAP_SETPCAP` (`PR_CAPBSET_DROP(2const)`).
- **`no_new_privs`** "is inherited across fork, clone, and execve and cannot be unset"; with it, setuid and setgid bits do not change IDs and "file capabilities will not add to the permitted set" (`Documentation/userspace-api/no_new_privs.rst`).
- **seccomp.** Installing a filter needs `no_new_privs` or `CAP_SYS_ADMIN`; filters are inherited by children and preserved across `execve`; with several filters the highest-precedence action wins, `ERRNO` above `ALLOW`; a filter "may not dereference pointers", so it sees only argument values (`Documentation/userspace-api/seccomp_filter.rst`; `seccomp(2)`). A `clone3` call's flags are in a structure passed by pointer (`clone(2)`). glibc 2.41 falls back from `clone3` to `clone` on `ENOSYS` (`sysdeps/unix/sysv/linux/clone-internal.c`).
- **io_uring** executes socket, connect, open and send operations submitted through a ring (`io_uring/opdef.c`, `IORING_OP_SOCKET`, `IORING_OP_CONNECT`, `IORING_OP_OPENAT`, `IORING_OP_SENDMSG`), which are not system calls a seccomp filter evaluates.
- **Landlock** (`Documentation/userspace-api/landlock.rst`, `include/uapi/linux/landlock.h`; ABI 6 at `security/landlock/syscalls.c`). A ruleset restricts "the thread enforcing it, and its future children", and enforcing one needs `no_new_privs` or `CAP_SYS_ADMIN`. ABI 4 restricts TCP bind and connect; ABI 5 restricts `ioctl` on character and block devices (`LANDLOCK_ACCESS_FS_IOCTL_DEV`); ABI 6 scopes connecting to abstract Unix sockets and sending signals to the domain (`LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET`, `LANDLOCK_SCOPE_SIGNAL`). A sandboxed thread "cannot modify filesystem topology, whether via mount(2) or pivot_root(2)". Rights are checked at `open`, so a descriptor opened before enforcement keeps its access. Landlock needs `CONFIG_SECURITY_LANDLOCK=y` and `landlock` in the active LSM list.
- **cgroup v2** (`Documentation/admin-guide/cgroup-v2.rst`). `memory.max` is the hard limit, reaching which invokes the OOM killer in the cgroup; `pids.max` is a hard limit on processes, enforced at `fork` and `clone`; `cpu.max` caps bandwidth per period. "For a kernel built with the CONFIG_RT_GROUP_SCHED option ... the cpu controller can only be enabled when all RT processes are in the root cgroup." `CLONE_INTO_CGROUP` (Linux 5.7) creates a child directly in a given cgroup (`clone(2)`).
- **nftables** `meta skuid` is the "UID associated with originating socket" (nftables 1.1.7, `doc/primary-expression.txt`, as packaged in Debian `nftables` 1.1.7-3); the kernel takes it from the socket file's credentials (`net/netfilter/nft_meta.c`, `nft_meta_get_eval_skugid`).

Candidates weighed, for who confines:

- **The supervisor, the same way for every VMM, with each VMM's own seccomp filter kept on as an inner layer** — chosen.
- **Each VMM's own mechanism**: the jailer and seccomp for Firecracker, seccomp and Landlock for Cloud Hypervisor. The jailer serves Firecracker only, Cloud Hypervisor's Landlock covers files at ABI 3 and not Unix sockets, and an in-house VMM would bring a third set. What a guest escape reaches would depend on which VMM the Machine runs, which ADR 0003 refused for fencing.
- **The jailer under the supervisor for Firecracker, the supervisor's own steps for the rest.** Two confinement paths to verify, one of which trusts all its inputs and chooses its own cgroup and device setup.
- **The supervisor alone, VMM filters turned off.** The VMMs' per-thread filters encode what each thread of that release calls, which nothing VMM-neutral can.

For the mechanisms:

- **A UID shared by every VMM.** Any VMM could attach any Machine's tap, since a tap admits its owner (`tun_not_capable`), and open any Machine's disk.
- **A user namespace per VMM.** The supervisor is already privileged and needs none to build the confinement; one would give the VMM a full capability set in its own namespace.
- **A network namespace for Firecracker by moving its tap into it, or a veth pair.** The tap would leave the host's routing, where ADR 0011 places it and ADR 0012 enforces the policy; that is a superseding decision for both.
- **A per-Driver seccomp allow-list applied by the supervisor.** It repeats, per VMM release, the list each VMM project keeps in step with its own code.
- **AppArmor or SELinux profiles.** Which one a node runs, if any, is the distribution's; Landlock needs no host policy and stacks with either.
- **CPU pinning (cpuset) and I/O limits.** Pinning is placement, not protection; `io.max` needs per-device numbers the node's storage decides.

## Decision

The supervisor confines every VMM process it starts, Machine and converter alike, by one sequence that takes its inputs from the Driver's description of the VMM and never from the VMM. The VMM's own seccomp filter stays on, and a Driver never turns it off. What each step leaves the VMM:

- **Its own user.** The node's configuration names a range of UIDs and GIDs that no other host account uses. mvmd allocates one UID, with the GID of the same number, to each Machine at Create Machine and Restore and to each conversion, records it durably, and releases it when the Machine is deleted or the conversion ends, once no VMM process of it exists. The VMM runs as it with no supplementary groups. The Machine's tap ([ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)), root disk and run directory are owned by it and by no other VMM's user; the image, kernel, initrd and VMM executable are readable and not writable by it.
- **Its own mount namespace.** The root is a new, empty tmpfs; the host's root is unmounted after `pivot_root`. It holds only:
  - the VMM executable, and the files of the boot description ([ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)), each bind-mounted read-only or read-write as the boot description marks it, and the host files the Driver names, read-only;
  - a `/dev` with only the device nodes the Driver names, each created with the host node's number and owned by the VMM's user, as the jailer does;
  - the Machine's run directory, bind-mounted read-write from its state directory. The VMM creates its API socket and its vsock socket there, which mvmd connects to by path from the host. Anything the VMM needs after it has started is placed there by mvmd: a disk attached while it runs, as a hard link to the disk file, and the files a snapshot writes, which mvmd moves into the snapshot store once written. The state directory is therefore one filesystem.
  - No `/proc` or `/sys` is mounted.
- **Its own network namespace, unless its Driver opens the tap by name.** A VMM with no network device (a converter) or whose Driver attaches the tap by descriptor (Cloud Hypervisor) runs in a new network namespace with only its loopback; the supervisor opens the tap in the host's namespace and passes the descriptor. A VMM that opens the tap by name (Firecracker v1.17.0) shares the host's network namespace, and mvmd's nftables table ([ADR 0012](./0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)) drops, in its inet output chain, every packet whose socket belongs to a UID in the node's VMM range. Frames the VMM writes to its tap enter at the tap and meet the tap's chains as before.
- **No capabilities.** Before it changes UID, the supervisor's child empties the bounding set and the ambient set; after it, the UIDs are all nonzero, so nothing is permitted and `execve` adds nothing.
- **`no_new_privs`**, set before the next two steps, both of which need it.
- **Landlock, ABI 6.** File system rights are handled in full and granted only beneath the paths of the mount namespace above, read-only where the bind mount is; `ioctl` is granted only on the device nodes the Driver names. TCP bind and connect are handled with no rule, so neither is allowed. The domain is scoped for abstract Unix sockets and for signals, so the VMM reaches no abstract socket and signals no process outside its own domain. mvmd refuses to start on a kernel that reports a Landlock ABI below 6.
- **A seccomp baseline**, installed last before `execve` and stacked under the VMM's own filters. It returns `EPERM` for `unshare`, `setns`, and `clone` with any namespace flag, and `ENOSYS` for `clone3`, whose flags a filter cannot read, so a caller falls back to `clone`, where they are checked. It allows everything else; mounting is already denied by Landlock.
- **Its own cgroup.** mvmd creates one cgroup v2 per VMM under a parent of its own, outside mvmd's cgroup, before the supervisor starts, and removes it after the VMM has ended. The supervisor creates the VMM in it with `CLONE_INTO_CGROUP`, so no instant passes with the VMM elsewhere; the supervisor itself is not in it. Limits, from the Machine's resources and the Driver's statement of its VMM's overhead:
  - `memory.max`: the guest memory plus the Driver's memory overhead;
  - `pids.max`: the Driver's task count for the Machine's vCPUs and devices;
  - `cpu.max`: the vCPU count plus the Driver's CPU overhead, in CPUs per period.

The order in the supervisor's child, each step failing the start before the VMM runs: open the descriptors the Driver passes; enter the new mount and network namespaces and build the root; drop capabilities and change GID and UID; set `no_new_privs`; enforce Landlock; install the baseline filter; `execve` the VMM.

### What each VMM needs open

Checked against Firecracker v1.17.0 and Cloud Hypervisor v54.0, as cited under Context.

| Need | Firecracker | Cloud Hypervisor |
|---|---|---|
| `/dev/kvm` | device node | device node |
| The tap | `/dev/net/tun` device node, host network namespace | descriptor, own network namespace |
| Kernel, initrd, disks | paths in the mount namespace | paths in the mount namespace |
| API socket | path in the run directory (`--api-sock`) | path in the run directory (`--api-socket path=`) |
| vsock | Unix socket path in the run directory (`uds_path`) | Unix socket path in the run directory (`socket=`) |
| On aarch64 | `/sys/devices/system/cpu/cpu0/cache` and `midr_el1`, read-only | `/sys/devices/system/cpu/cpu0/cache`, read-only |
| Not given | `/dev/urandom` (MMDS version 2, unused) and `/dev/userfaultfd` (lazy restore, not yet decided) | `/dev/vhost-vsock`, which it does not open |

### How a Driver states its VMM's needs

A Driver's description of the VMM process ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)) carries, in VMM-neutral terms: the executable; the host files it reads beyond the boot description; the device nodes it opens; whether it takes its network device by descriptor; and its memory, task and CPU overhead. The supervisor applies every step above from these alone, so a new VMM is confined by writing its Driver, with no supervisor code. Nothing in the description grants a capability, chooses the UID, or skips a step.

## Consequences

- A guest escape reaches its own Machine's files, devices, sockets and tap, under one unprivileged UID, and nothing of another Machine's. What it reaches no longer depends on which VMM the Machine runs, except for the network namespace under Firecracker.
- Under Firecracker the VMM shares the host's network namespace. The output rule keeps its sockets from sending IP traffic and Landlock keeps it from host abstract Unix sockets; what an unprivileged process can read about the host's network through a socket that carries no IP traffic, netlink among them, was not checked and is not kept from it.
- io_uring operations are not seen by seccomp filters, the VMM's or the baseline. Namespaces, Landlock, the UID and the output rule apply to them; the baseline does not.
- Host requirements grow: Landlock ABI 6 enabled (Linux 6.12 or later with `landlock` in the LSM list), cgroup v2 with the memory, pids and cpu controllers, and a UID and GID range for VMMs. On a kernel built with `CONFIG_RT_GROUP_SCHED`, the cpu controller can be enabled only while every real-time process is in the root cgroup, so there the real-time supervisors ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)) are placed in the root cgroup.
- The supervisor needs, until its child execs the VMM, `CAP_SYS_ADMIN` for the namespaces and `pivot_root`, `CAP_SETUID`, `CAP_SETGID`, `CAP_SETPCAP`, `CAP_MKNOD` and `CAP_CHOWN`, beyond what ADR 0003 lists. The VMM holds none of them.
- Every start and restore builds a mount namespace, so start latency includes it.
- A VMM exhausting its memory is OOM-killed inside its own cgroup, and the Machine stops; the Driver's overhead figures must cover the VMM's peak, not its average.
- I/O is not limited per Machine; a Machine can still use the node's disk bandwidth.
- Files a running VMM needs later reach it only through its run directory, so hot attach is a hard link and the state directory cannot span filesystems.

## Signs the decision was wrong

- A guest escape test reaches a file, socket, device or process outside the VMM's own Machine.
- A VMM admitted later cannot run under the baseline filter, because it needs `clone3` without a fallback or creates namespaces itself.
- Driver descriptions gain fields only one VMM uses, which the supervisor has to interpret, so confinement depends on the VMM again.
- Building the mount namespace is a noticeable share of Start or Restore at the node's Machine density; Firecracker's jailer documents jail creation slowing with the number of mount points and concurrent jails (`docs/jailer.md`, Known limitations).
- Machines are OOM-killed under normal load because the Driver's overhead figures are too low, or the figures are set so high that the limit protects nothing.
- Nodes that mvmd must serve cannot provide Landlock ABI 6.

## Errata

- 2026-11-10. The decision stands. Context, "Cloud Hypervisor's devices", and the table in §What each VMM needs open give `/dev/kvm` as the one device node Cloud Hypervisor opens, and give no node it is not handed. That was incomplete: it also opens `/dev/urandom`, the source of the virtio-rng device it adds by default (at v54.0: `vmm/src/vm_config.rs`, `RngConfig::DEFAULT_RNG_SOURCE`; `vmm/src/device_manager.rs`, where the device is created with that path; `virtio-devices/src/rng.rs`, `Rng::new`, `File::open`). A boot of v54.0 in a root without the node failed with "Cannot create virtio-rng device" (#140). The Cloud Hypervisor Driver names `/dev/kvm` and `/dev/urandom` as the device nodes the VMM opens.
