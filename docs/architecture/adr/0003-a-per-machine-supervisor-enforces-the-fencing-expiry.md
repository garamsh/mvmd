# ADR 0003: A per-Machine supervisor enforces the fencing expiry

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#8 and decided in review of the pull request that adds this ADR.

`runtime-api.md` §Fencing deadline, before this ADR, fixed the guarantee: a VM never runs past the recorded expiry, whether or not the mvmd daemon process is running, across a crash, a kill and a restart of mvmd alike. The expiry is held against a clock that keeps counting while mvmd is down and cannot be set, tied to the identity of the host's current boot. The mechanism was left open, with a per-VM supervisor or the VMM's own launcher holding the expiry as the named candidates. Three further constraints:

- Whatever counts the expiry while mvmd is down must not, by its own death, leave a VM unfenced.
- It must not depend on which VMM runs: an in-house or out-of-process VMM must be admissible (ADR 0001, VMM extensibility).
- No container or container runtime (ADR 0001).

The Linux behaviour the decision rests on, from Linux man-pages 6.9.1 unless another source is named:

- **Where a guest executes.** `KVM_RUN` "is used to run a guest virtual cpu"; VM ioctls are issued from the process that created the VM, and vcpu ioctls from the thread that created the vcpu (Linux v6.12, `Documentation/virt/kvm/api.rst`, §1 General description and §4.10 KVM_RUN). A guest therefore executes only while a thread of its VMM process is inside `KVM_RUN`, and executes nothing once that process has ended, whichever VMM it is.
- **PID namespace init.** "If the "init" process of a PID namespace terminates, the kernel terminates all of the processes in the namespace via a SIGKILL signal" (`pid_namespaces(7)`). Membership "is determined when the process is created and cannot be changed thereafter", and a process may not enter an ancestor namespace, so a process cannot leave the namespace by forking, daemonizing or creating a nested namespace. From an ancestor namespace, SIGKILL and SIGSTOP are "forcibly delivered" to the init process; no other signal is unless it installed a handler. Creating one needs `CAP_SYS_ADMIN` (`clone(2)`, `CLONE_NEWPID`).
- **`PR_SET_PDEATHSIG`.** The signal is sent when the *thread* that created the process terminates, not the whole parent process; it is cleared on exec of a set-user-ID, set-group-ID or capability-bearing binary and on any change of effective or filesystem user or group ID; and if the parent has already died when it is set, no signal is ever sent (`PR_SET_PDEATHSIG(2const)`).
- **Clocks.** `CLOCK_MONOTONIC` is not settable and "does not count time that the system is suspended". `CLOCK_BOOTTIME` is identical "except that it also includes any time that the system is suspended" (`clock_getres(2)`). Both are unaffected by a stepped wall clock. A timer set on `CLOCK_BOOTTIME` with `TFD_TIMER_ABSTIME` expires when that clock reaches the value given (`timerfd_create(2)`). Both clocks take per-namespace offsets inside a time namespace (`time_namespaces(7)`).
- **Boot identity.** `/proc/sys/kernel/random/boot_id` is a random string "generated once" (`random(4)`), at boot, so each boot carries its own.
- **pidfd.** `pidfd_open(2)` (Linux 5.3) refers to the process itself, not to its PID number; it becomes readable when the process terminates, and `pidfd_send_signal(2)` signals exactly that process.
- **cgroup kill.** Writing `1` to `cgroup.kill` sends SIGKILL to every process in the cgroup and its descendants (Linux v6.12, `Documentation/admin-guide/cgroup-v2.rst`, §4-3 Core Interface Files). Something alive has to write it.
- **Scheduling.** Timer slack "is not applied to threads that are scheduled under a real-time scheduling policy" (`PR_SET_TIMERSLACK(2const)`). With the reset-on-fork flag, children "do not inherit privileged scheduling policies"; real-time tasks share 0.95 s of each 1 s period by default (`sched(7)`). `mlockall(2)` locks a process's pages so it takes no page-in on its own path.
- **Service managers.** systemd's default `KillMode=control-group` kills every remaining process in a unit's control group when the unit stops (`systemd.kill(5)`, systemd 257.13).

containerd keeps a shim per container, or per group of containers, that outlives the daemon; on boot containerd reconnects to its shims and, where it cannot connect to one, runs the shim's `delete` to clean up after it — "This happens if a shim is SIGKILL'd with a running container" (containerd v2.0.0, `core/runtime/v2/README.md`). There, a shim's death leaves cleanup to the daemon. Here, it must leave no VM running.

Candidates weighed:

- **A supervisor per Machine that is init of its VMM's PID namespace** — chosen.
- **The VMM's own launcher holding the expiry.** Enforcement would be a feature every VMM's launcher has to provide and be verified for, an in-house VMM would have to reimplement it, and the process being fenced would be the one trusted to stop itself.
- **The in-process Driver holding the expiry.** In-process Drivers die with mvmd (`driver.md`, in-process first), which is the case the guarantee exists for.
- **A supervisor tied to its VMM by `PR_SET_PDEATHSIG`.** A VMM or launcher that drops privileges or execs a capability-bearing binary clears the signal, and a supervisor that dies before the call is made sends nothing; either leaves a VM running with no one counting. The signal also follows the forking thread rather than the process, so in a multi-threaded supervisor it can stop a VM while its supervisor lives.
- **One enforcer per node holding every VMM.** Its crash or its upgrade would stop every VM on the node, and it would have to launch every VMM on mvmd's request, a process manager rather than a timer.
- **A cgroup per Machine, killed at expiry.** `cgroup.kill` needs a live writer, so it does not cover the death of whatever holds the timer.
- **A service manager's runtime limit** (a unit with a maximum run time). The deadline's clock and its renewal would be the service manager's, and a host without that manager would have no fence.
- **A host watchdog that reboots on missed renewal.** An mvmd restart, which VMs are meant to survive, would reboot the host.
- **The Guest Agent stopping its own guest.** The guest is untrusted (#2 §5, security).
- **mvmd alone, restarted by a service manager.** Nothing counts while it is down.

## Decision

Each Machine whose VMM process exists runs under its own supervisor, `mvmd-supervisor`, a separate executable mvmd starts. The supervisor is the init process of a new PID namespace and starts the VMM inside it, so the VMM and everything it starts die with the supervisor. It receives the current expiry as part of its own start, holds it as an absolute `CLOCK_BOOTTIME` value, and ends its namespace when that clock reaches it. mvmd renews by passing each supervisor the new expiry.

- **Which process counts.** The supervisor, one per Machine whose VMM process exists. It shares mvmd's time namespace, so an absolute value means the same to both, and the value crosses as an absolute time so that delivery delay cannot lengthen it. It holds the expiry in memory only: its death takes the VM with it, so nothing outlives a lost value.
- **No VMM without an expiry.** The supervisor is given the current expiry when mvmd starts it, and refuses to start the VMM without one or with one that has passed. Every start and every restore is a new supervisor, so no VMM process ever exists under a supervisor that is counting nothing. Starts and renewals are applied one at a time, so every supervisor that exists when a renewal is applied receives it.
- **Its own death.** The kernel kills every process in the namespace when its init ends, whatever killed it and whatever the VMM did to its credentials or process tree.
- **How it acts at the expiry.** It exits, and the kernel kills the namespace. It runs under a real-time scheduling policy with reset-on-fork, so the VMM it starts does not inherit that policy and cannot delay it, and with its memory locked, so its path to exit takes no page-in.
- **mvmd acts too.** While running, mvmd holds the same expiry and at it kills every supervisor through its pidfd. Either one suffices; the second covers a supervisor that is alive but not acting.
- **How mvmd renews.** An extension is recorded durably first and then passed to every supervisor; a shortening is passed to every supervisor first and then recorded. At every instant no supervisor holds an expiry later than the recorded one. The acknowledgement follows both. A supervisor that does not take an extension keeps the earlier expiry, which can only stop its VM early; one that does not take a shortening is killed before the shortening is recorded.
- **Clock, reboot and stepped wall clock.** `CLOCK_BOOTTIME`, not `CLOCK_MONOTONIC`: a suspended host leaves the client's clock running, and a clock that paused would let a VM resume with time it no longer has. Not `CLOCK_BOOTTIME_ALARM`: a suspended guest executes nothing, so waking the host gains nothing and needs `CAP_WAKE_ALARM`. mvmd records the expiry with the `boot_id` of the boot it was taken in; a different `boot_id` at start means a reboot, which no supervisor or VM survives, and the deadline is not armed. The wall clock is never read for the deadline.
- **Reattach after an mvmd restart.** Each supervisor listens on a socket in its Machine's state directory. Before serving any call, mvmd connects to each, takes a pidfd to it and to its VMM, compares the recorded expiry with `CLOCK_BOOTTIME`, kills every supervisor if it has passed, and otherwise passes the recorded expiry to each. It then reattaches to the VMMs through the Driver's `List`. A supervisor mvmd cannot reach still enforces what it holds.
- **What the Driver contract gains.** The Driver describes the VMM process to start and mvmd starts it under the supervisor; the Driver never starts a VMM process itself, every process a Machine needs runs under its supervisor, and no VMM process runs under a real-time policy. The Driver never sees the expiry. The VMM is reached as before, through the interface it exposes.
- **What mvmd needs to talk to.** One local conversation, mvmd to each supervisor: pass an expiry, read what the supervisor holds and which Machine it serves. It is carried on the transport chosen for the runtime API in garamsh/mvmd#7, and is defined by the daemon issue that implements this.
- **Placement.** The supervisor runs outside mvmd's cgroup, so that a service manager stopping mvmd does not stop it.

### Failure cases

δ is the time from `CLOCK_BOOTTIME` reaching the expiry to the end of the supervisor's VMM: the wake-up of a real-time thread with no timer slack and locked memory, its exit, and the kernel's SIGKILL of its namespace. It is finite because nothing the VM does can delay it — the VMM runs under no real-time policy, and the supervisor uses a negligible share of the real-time budget. It is not a figure: no number for it is measured, and it is measured with the other prototype numbers (`runtime.md` §Open questions, density and restore numbers). "Past the expiry" below is guest execution after the expiry.

| Case | What happens | Longest a VM runs past the expiry | Why bounded |
|---|---|---|---|
| mvmd killed or crashed, supervisors alive | Supervisors count on; at the expiry each ends its namespace. On restart mvmd reattaches and never extends a passed expiry. | δ | The supervisor depends on nothing of mvmd's. |
| A supervisor killed, mvmd alive | The kernel kills its VMM at once; mvmd sees both pidfds readable and reports the Machine Stopped. | δ, and 0 when the death comes before the expiry | The namespace dies with its init. |
| mvmd and a supervisor both killed | As the previous case; mvmd plays no part in it. | δ, and 0 when the death comes before the expiry | The namespace dies with its init. |
| Host reboot | No process survives it. On start mvmd sees a new `boot_id`, treats the deadline as not armed and starts nothing. | δ, up to the reboot; nothing runs after it | Before the reboot the other cases hold; after it no VM exists. |
| mvmd dies while starting a Machine | If mvmd dies before the supervisor starts, no VMM process exists. Once the supervisor has started, it holds the expiry it was started with and counts it, whether or not it has started the VMM. On restart mvmd finds the supervisor by its socket and reattaches as for any other. | δ | The supervisor has an expiry before any VMM process exists. |
| Renewal acknowledged just before mvmd crashes | The acknowledgement follows the durable record and the push, so every supervisor already holds the acknowledged expiry. | δ past the acknowledged expiry | The supervisors enforce what the client was told. |
| mvmd crashes during a renewal, before acknowledging | Each supervisor holds an expiry no later than the recorded one. On restart mvmd passes the recorded one; the client resolves the unknown outcome by repeating the renewal. | δ past the recorded expiry | The ordering rule. |
| Host suspended across the expiry | The guest executes nothing while suspended; `CLOCK_BOOTTIME` counted the suspension, so the timer is due at resume. | δ after resume | The clock counts suspension. |
| Wall clock stepped | No effect. | δ | Neither clock is settable. |
| A supervisor unreachable by mvmd but alive | It enforces the expiry it last took, which is no later than the recorded one. | δ | The supervisor needs no renewal to act. |
| A supervisor stopped or traced by a privileged host process | mvmd, if running, kills it through its pidfd at the expiry. | δ plus mvmd's own reaction time while mvmd runs; no bound with mvmd also down | Outside the guarantee: only a process with root-equivalent power over the supervisor can stop it, and such a process can run a VM by itself. |

## Consequences

- The guarantee holds to within δ, and against failures of mvmd, the supervisor and the host, not against a privileged host process acting on the supervisor. `runtime-api.md` §Fencing deadline states it so, and the client's margin covers δ.
- One more process per running Machine, with locked memory and a real-time policy.
- Every renewal reaches every supervisor before it is acknowledged, so renewal latency grows with the number of Machines on the node.
- Fencing is independent of the VMM: an in-house or out-of-process VMM is fenced as soon as its Driver describes its process, with no enforcement code of its own. An out-of-process Driver protocol must keep the VMM process started by the supervisor.
- Supervisors outlive an mvmd upgrade, so mvmd speaks to supervisors started by an earlier release, and a supervisor is upgraded only when its Machine is next started.
- Privileges and kernel. mvmd needs `CAP_SYS_ADMIN` to create the supervisor's PID namespace (`clone(2)`), and a kernel with `pidfd_open` (Linux 5.3). The supervisor needs `CAP_SYS_NICE`, or a nonzero `RLIMIT_RTPRIO` soft limit at or above the priority it takes, to set its real-time policy (`sched(7)`, Privileges and resource limits), and `CAP_IPC_LOCK`, or an `RLIMIT_MEMLOCK` soft limit covering its memory, for `mlockall` (`mlockall(2)`, Limits and permissions).
- A PID namespace is used, and no container: the microVM stays the isolation and execution unit, no OCI runtime is involved, and the namespace holds only the supervisor and its VMM. This reads ADR 0001's "no container as an isolation or execution unit" as not reaching a kernel namespace used for process lifetime.
- The kill ends guest execution. Host I/O the VMM had already submitted to the kernel may complete after it, which matters once a disk can be reached from another node.
- Deploying mvmd under a service manager must keep supervisors out of mvmd's cgroup; a deployment that does not loses VMs on every mvmd restart, though never past the expiry.

## Signs the decision was wrong

- A test that kills mvmd, a supervisor or both sees guest execution past the expiry by more than the measured δ.
- A VMM cannot run inside a PID namespace whose init it is not, or needs a process outside its supervisor's namespace.
- Renewal latency at the node's Machine density approaches the deadline lengths clients choose.
- The range of supervisor releases mvmd must speak to keeps widening, because Machines are rarely restarted.
- δ measured under load is of the order of the deadline's margin, so a real-time policy and locked memory are not what bounds it.
