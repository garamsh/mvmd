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
   - A live supervisor that no Running or Paused record claims is terminated, with its network withdrawn: nothing promised it, and no call could reach it (`restore.go:111`).
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

The state directory holds `mvmd.db`, the disks, the snapshots, the boot sets and the image cache, and each is lost with the directory ([README.md](./README.md) §What mvmd keeps). Nothing in the repository backs it up ([README.md](./README.md) §What an operator cannot do today), so what follows is what a start does, not a procedure that restores.

- **`mvmd.db` lost or empty, the rest of the directory present.** At the next start no record claims a supervisor that still runs, so mvmd terminates each of them, which ends every Machine's VMM, and removes the state kept for each Machine the store does not hold (`daemon/internal/machine/restore.go:111` and `:135`). The disks stay as files in `disks/`. A disk the runtime API's Create disk made is listed by List disks with no holder and can be attached to a new Machine; a Machine's root disk belongs to its Machine, and no call adopts one whose record is gone. If `mvmd.db` can be recovered from a copy that was taken while mvmd was stopped, put it back before the start, and the start restores from it as above; a copy older than the files beside it has not been tried.
- **The directory lost with its file system.** Nothing is left to recover on this node. The client's own desired state decides what runs elsewhere, and the images are pulled again.
- **Before you start mvmd on a directory you doubt**, stop the Machines you can still reach by their client, since the start will end the ones it cannot account for.

## What the client does

mvmd reports and executes, and the decision to run, wake or stop a Machine is its client's (`docs/architecture/runtime.md` §Boundary with gaon). After any of the above the client reads the node, compares it with its own record of what should run, renews the deadline, and starts what it wants running.
