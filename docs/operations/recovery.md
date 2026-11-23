# Recovering a node

The steps after mvmd crashes, after the host reboots, and when the state directory is damaged or lost: what each leaves, what mvmd settles by itself at its next start, and what the operator does.

Checked as `README.md` states. Every step here rests on reading the code and the architecture documents it cites; none was run against a failure of a real node.

## Contents
- After mvmd crashes
- After the host reboots
- A record that cannot be read
- A state directory that is lost
- What the client does

## After mvmd crashes

A crash or a kill of mvmd leaves each Machine's supervisor and VMM running, as a stop does ([upgrade.md](./upgrade.md) §What a restart keeps and what it loses), and the fencing deadline counts on. So the first act is the clock: the Machines run until the recorded expiry, and a client that cannot reach mvmd cannot renew it.

1. **Get mvmd running again before the expiry.** With the unit of [install.md](./install.md), `Restart=on-failure` starts it again. If it does not stay up, read the journal: a refused configuration or host ends the process with exit code 1 and one line on standard error (`daemon/internal/app/app.go:338`), and the fix is in that line.
2. **Let it restore.** At its start mvmd, before it opens the socket, rebuilds from `mvmd.db` and compares each Machine with the live supervisors (`daemon/internal/machine/restore.go:22`, read):
   - A Machine recorded Running or Paused whose supervisor lives keeps its state.
   - One whose supervisor is gone is Stopped, with the reason "the VMM ended while mvmd was not running", and the cause the console shows (`restore.go:13`). A Machine created with `restart_on_boot` is started again only when the VMM ended with the host's boot, not for this ([After the host reboots](#after-the-host-reboots)).
   - If the supervisors cannot be listed at all, every Machine that claimed to run is Faulted with the reason, since mvmd cannot tell (`restore.go:94`).
   - A live supervisor that a record holds but no Running or Paused record claims is terminated, with its network withdrawn: its start never reported success. One that no record holds at all is left running and reported as an orphan ([A state directory that is lost](#a-state-directory-that-is-lost); `restore.go:111`).
   - A Running or Paused Machine whose network cannot be made again is Faulted with the reason "the Machine's network could not be restored after mvmd restarted" (`restore.go:16`).
3. **Compare the deadline.** If the expiry has passed while mvmd was down, mvmd stops every Machine first, and starts none by itself; a renewal arms the deadline again and the client decides what runs (`docs/architecture/runtime-api.md` §Fencing deadline).
4. **Look at the Faulted Machines.** mvmd could not establish a Faulted Machine's state, and the reason says why. The only calls that apply are stop, delete, inspect and reads, and Delete ends its VMM if one remains (`docs/architecture/runtime-api.md` §Machine states; `restore.go:121`). Whether to delete it, or to read its disks first, is the client's: the files are in `disks/` until it does.

## After the host reboots

No process survived. mvmd starts, finds that the host's `boot_id` is not the one in its record, and treats the deadline as not armed (`docs/architecture/runtime.md` §Fencing).

1. **The unit starts mvmd, and its `ExecStartPre` makes the supervisors' cgroup again**, since `/sys/fs/cgroup` is not persistent ([install.md](./install.md) §Install the unit). The host's forwarding sysctls are the host's to set at boot, and mvmd refuses to start where they read otherwise (`docs/architecture/runtime.md` §Network).
2. **Each Machine that was Running or Paused is Stopped** at that start, with the reason that its VMM ended, as above. Its disks and its record are kept.
3. **A Machine created with `restart_on_boot` that was Running when the host went down is started again**, once, at the first arming of the deadline in the new boot, and a Stop, Pause or Delete asked after it last ran cancels this. A restart that fails leaves it Stopped with a reason that begins "the restart after the host's boot changed failed", and it is not tried again (`docs/architecture/adr/0062-a-machine-created-with-restart-on-boot-that-was-running-when-its-host-went-down-is-started-again-at-the-first-arming-in-the-new-boot.md`).
4. **Arm the deadline.** The client renews it; Start and Restore are refused until it is armed. A Machine created `fenced` also needs its own fence renewed: Start refuses it until then, and a fenced Machine marked `restart_on_boot` waits for its fence without holding back the others (`docs/architecture/adr/0064-a-machine-created-fenced-has-a-fence-of-its-own-that-ends-only-its-vmm-when-it-lapses.md`).
5. **The client starts the rest.** mvmd decides nothing more (`docs/architecture/adr/0033-a-vmm-that-ends-on-its-own-moves-its-machine-to-stopped-with-the-reason.md`).

## A record that cannot be read

If the bytes of a Machine's record cannot be decoded, that Machine is Faulted with the reason "the Machine's record could not be read" and the decoding error; the stored bytes are not changed, and the other records are read as usual (`daemon/internal/machine/restore.go:45`). Its VMM, if it is running, is left alone, since mvmd does not know its state, and Delete ends it (`restore.go:121`).

If mvmd cannot open `mvmd.db` at all, it does not start. Stop whatever holds the file: one process holds it, and a second mvmd fails after a second with `open .../mvmd.db: timeout`. If the file itself is damaged, go on to the next section.

## A state directory that is lost

The state directory holds `mvmd.db`, the disks, the snapshots, the boot sets and the image cache, and each is lost with the directory ([README.md](./README.md) §What mvmd keeps).

**Back it up before it is lost.** With mvmd stopped, as for an upgrade ([upgrade.md](./upgrade.md) §Before you stop mvmd), run `mvmd -backup <file> -config <config>`. It writes a consistent copy of `mvmd.db`, read-only, to a file that does not exist, and fails while another mvmd runs on the directory. Copy the file off the host. The disks of Machines that were running when it was taken are being written, so a copy of them is crash-consistent at best, and this does not back them up. To put a copy back, stop mvmd, place it as `mvmd.db` of the state directory with mode 0600 and the owner of the directory, and start. A copy older than the files beside it has not been tried (`docs/architecture/adr/0071-a-start-reconciles-what-it-finds-and-leaves-a-supervisor-no-record-holds-running-as-an-orphan.md`).

- **`mvmd.db` lost or empty, the rest of the directory present.** A supervisor no record holds is an orphan: mvmd leaves it running, logs "a supervisor no record holds was found and left running", lists it in `orphaned_supervisors` of runtime info, and emits a `SupervisorOrphaned` event of its identifier. The Machines are not ended. They run until the operator ends each supervisor on the host or their fencing expiry passes, with no network, since the network reconciliation deletes the taps of Machines the records do not hold. No call names an orphan or adopts it. The disks stay as files in `disks/`: a disk the runtime API's Create disk made is listed by List disks with no holder and can be attached to a new Machine, and a Machine's root disk belongs to its Machine, whose record is gone. An identifier an orphan holds cannot be created while it runs. When an orphan has ended, the next start sweeps the state directory it left.
- **The directory lost with its file system.** Nothing is left to recover on this node. The client's own desired state decides what runs elsewhere, and the images are pulled again.

## What the client does

mvmd reports and executes, and the decision to run, wake or stop a Machine is its client's (`docs/architecture/runtime.md` §Boundary with gaon). After any of the above the client reads the node, compares it with its own record of what should run, renews the deadline, and starts what it wants running.
