# Dispatch a worker

The acts that start a worker as a Claude Code subagent on this host, confirm the start, reuse the worker across rounds, judge it by what it produced, and release it. No convention in `docs/convention/` governs dispatch. The role a worker holds is `AGENTS.md` §Roles, and the branch it works on is `docs/convention/git.md` §Branches.

## Host facts

Read from Claude Code 2.1.310 and git 2.47.3 on this host. Each fact below names its reading and when it was taken. A behaviour that no reading covers is marked "not measured" where the procedure meets it.

- **Start.** The Agent tool, called with `subagent_type: "worker"`, `isolation: "worktree"` and `run_in_background: true`, made a worktree at `<repo>/.claude/worktrees/agent-<agent id>` with `HEAD` at `main`'s tip, on a branch the host named `worktree-agent-<agent id>`. The PM read this with `git worktree list --porcelain` at 2026-10-25T18:12:21Z (2026-10-26 03:12 +0900), right after starting the #579 worker (#581, comment of 2026-10-25T18:12:43Z). The start was confirmed by the worktree existing, not by the Agent tool's launch result.
- **Role.** `~/.claude/agents/` holds `pm.md`, `qa.md` and `worker.md`, and `worker.md`'s frontmatter reads `name: worker`. Read by the #579 worker with `ls` and `head` at 2026-10-25T18:19:59Z (2026-10-26 03:19 +0900). That `subagent_type: "worker"` selects this file is inferred from the matching `name`. No source read here states the mapping.
- **Lock.** The only `locked` reading is the start reading above, taken at 2026-10-25T18:12:21Z (2026-10-26 03:12 +0900) while round 1 ran. `git worktree list --porcelain` read `locked claude agent agent-<agent id> (pid <PM pid> start …)`, naming the PM's PID and not the worker's. From about 18:18Z (03:18 +0900), no reading found a lock:
  - the base checkout at about 18:18Z (03:18 +0900), read by the PM;
  - the base checkout at 2026-10-25T18:22:15Z (2026-10-26 03:22 +0900), read by the PM;
  - the worktree at 2026-10-25T18:19:59Z (2026-10-26 03:19 +0900), read by the #579 worker during its resumed second round.

  Each printed no `locked` line. The 18:22:15Z and 18:19:59Z readings also found no `locked` file in the worktree's directory under `git rev-parse --git-common-dir`'s `worktrees/` (#581, comment of 2026-10-25T18:18:38Z, corrected at 2026-10-25T18:23Z). When the lock goes, and why, is not established.
- **Untracked worktrees.** At the start reading above, `git status --porcelain --untracked-files=all` in the base checkout printed `?? .claude/worktrees/agent-<agent id>/`, and `git check-ignore -v .claude/worktrees` exited 1. `.gitignore` now carries `.claude/worktrees/`. In a worktree holding no `.claude/worktrees` directory, at 2026-10-25T18:21:12Z (2026-10-26 03:21 +0900), `git check-ignore -v .claude/worktrees/` and `git check-ignore -v .claude/worktrees/agent-x/file` each printed `.gitignore:27:.claude/worktrees/` and exited 0. The bare `.claude/worktrees` exited 1 there, because a pattern ending in `/` matches only a directory and none existed at that path. At 2026-10-25T18:24:52Z (2026-10-26 03:24:52 +0900), with `main` at `b9733c6fd5993667d415aaab72f58abbd1ce50d5` and `.claude/worktrees/agent-<agent id>/` still on disk, `git status --porcelain --untracked-files=all` in the base checkout printed nothing, read by the PM (#584). The entry omits the worktrees there.
- **Worker isolation.** The harness refuses a worker's git command aimed at the base checkout. At about 2026-10-25T18:19Z (2026-10-26 03:19 +0900), the #579 worker's `git -C <repo> worktree list --porcelain` was refused with "this command redirects git to the shared checkout via -C. Refusing to run it". The base checkout's own reading — its status, and the lock — is therefore the PM's to take.
- **Completion notice.** Both completion notices of the #579 round named `worktreeBranch` `worktree-agent-<agent id>`, while `git -C <worktree> branch --show-current` read the assigned branch `docs/anchor-self-recording-basis`. The worker had created the assigned branch rather than renaming, so `git branch --list` at 2026-10-25T18:18:23Z (2026-10-26 03:18 +0900) still listed `worktree-agent-<agent id>` (#581, comment of 2026-10-25T18:18:38Z).
- **Reuse.** A SendMessage to the completed worker, addressed by its agent id, returned "Resuming agent …". The worker answered review on the same branch, in the same worktree, and with the first round's context. `git worktree list --porcelain` showed no new worktree before or after. Read by the PM between 2026-10-25T18:12Z and 18:18:23Z (2026-10-26 03:12 and 03:18 +0900) (#581, comment of 2026-10-25T18:18:38Z). For the next round, the resumed worker ran `git fetch origin && git switch -c docs/subagent-dispatch-procedure origin/main` in the same worktree. It printed "Switched to a new branch 'docs/subagent-dispatch-procedure'", and `git rev-parse HEAD` read `7396bf97ed6300005bc81c78cdf68985af0c3cb4`, at about 2026-10-25T18:19Z (2026-10-26 03:19 +0900).
- **Remote branch on merge.** After PR #582 merged, `git ls-remote --heads origin docs/anchor-self-recording-basis` printed nothing (#581, comment of 2026-10-25T18:18:38Z). The repository's `delete_branch_on_merge` deletes the remote branch.
- **Release.** The PM released the #579/#581 worker from the base checkout once its turn had ended, and recorded each step in #584:
  - **Processes.** At 2026-10-25T18:25:03Z (2026-10-26 03:25:03 +0900), `find /proc -maxdepth 2 -name cwd -lname '<worktree path>*'`, run from the base checkout with no exclusion of its own process chain, printed nothing. The scanning shell's cwd was outside `<worktree path>`, so the scan could not match itself (#584, comment of 2026-10-25T18:29Z). The positive control was the same command, also without exclusion, run from a subshell `cd`'d into the worktree, which printed 4 lines, the scan's own processes; the scan sees a process that is there, so the empty reading is an absence. The worker had started no background processes.
  - **Worktree status.** At 2026-10-25T18:25:03Z (2026-10-26 03:25:03 +0900), `git -C <worktree> status --porcelain --untracked-files=all` printed nothing.
  - **Worktree removal.** `git worktree remove <worktree path>`, without `--force`, exited 0. At 2026-10-25T18:25:22Z (2026-10-26 03:25:22 +0900), `git worktree list --porcelain` listed only the base checkout, and `.claude/worktrees/` was empty. No lock blocked the removal.
  - **Branches.** At 2026-10-25T18:25:22Z (2026-10-26 03:25:22 +0900), in the command that took the worktree-removal confirmation, `for n in 582 583; do gh pr view $n --json number,state,headRefName ...; done` printed `582 MERGED docs/anchor-self-recording-basis` and `583 MERGED docs/subagent-dispatch-procedure`, and `git log --oneline main..worktree-agent-<agent id> | wc -l` printed `0` (#584, comment of 2026-10-25T18:29Z). After that and before 2026-10-25T18:26:28Z (2026-10-26 03:26:28 +0900), the `date -u` of the PM's next command, two separate commands ran, neither printing its own instant. First, `git branch -D docs/anchor-self-recording-basis docs/subagent-dispatch-procedure worktree-agent-<agent id>` deleted all three branches, printing for each the abbreviated tip it held. Second, `git branch --list` listed only `main`, and `git ls-remote --heads origin <branch>` printed nothing for each branch. The three tips resolve to `5134b072a6f8a0fd2847bd55d3b74a9239aa9958`, `47cf0395e8bb8ab459a44e9d80479021e3242dd9` and `7c4e24ff4b970958d537072579c2f6782bf1b3f3`, read by the #584 worker at about 2026-10-25T18:27Z (2026-10-26 03:27 +0900) with `git rev-parse` on the printed prefixes; the first two match the `headRefOid` that `gh pr view 582 --json headRefName,headRefOid` and `gh pr view 583 --json headRefName,headRefOid` returned then.
- **Removing a worktree.** `git help worktree` (git 2.47.3), read at 2026-10-25T18:20Z (2026-10-26 03:20 +0900), states under `remove`: "Only clean worktrees (no untracked files and no modification in tracked files) can be removed." Under `--force` it states: "To remove a locked worktree, specify --force twice."

## Start

1. Write the brief. It names:
   - the role from `AGENTS.md` §Roles the worker holds;
   - the issue it implements;
   - the base commit, as a full object name;
   - the branch name per `git.md` §Branches, which the worker creates from `origin/main` before its first push, since the host names the worktree's branch itself;
   - the paths it may change.

   Where the change edits a convention file to meet an obligation another convention file states, the brief asks for the citation of that obligation in the pull request body and not in the convention file: `docs/convention/README.md` §Independence, "A convention file does not send the reader to another convention file", and §Stack-specific, "the files do not point at each other".
2. Start it with the Agent tool: `subagent_type: "worker"`, `isolation: "worktree"`, `run_in_background: true`, and the brief as its prompt. Keep the agent id the result returns; §Reuse and §Release address the worker by it.

## Confirm the start

A launch result is the tool's report, not an observation. Ask the host:

1. **The worktree exists.** `git worktree list --porcelain`, run from the base checkout, lists `<repo>/.claude/worktrees/agent-<agent id>` with `HEAD` at `main`'s tip.
2. **The role took.** Not measured. No reading here shows a running background subagent's reads before it reports. Its report and pull request are the first evidence, and the template's "Conventions consulted" field is the one read against the diff.

## Reuse

1. Send the next round with SendMessage, addressed by the agent id. Name the issue, the base commit, the assigned branch, and the paths. The worker cuts the new branch from `origin/main` in the worktree it already holds.
2. Confirm that `git worktree list --porcelain` lists no worktree it did not list before the message.

## Judge by output

A completion notice is the worker's report. The branch it names is the one the host chose at start (§Host facts), so ask git and the forge:

- the branch the worktree is on: `git -C <worktree> branch --show-current`;
- commits on it: `git log origin/main..origin/<branch>`;
- the pull request and its head: `gh pr list --head <branch> --json number,state,headRefOid`;
- a comment on its issue.

## Release

One release has been run against a subagent's worktree, of a worker whose worktree was clean and unlocked (§Host facts). Release of a worker whose worktree is locked or dirty is not measured. Run each as its own command, and confirm it by asking the host afterwards, not by the command's exit status.

1. **Processes.** Confirm that nothing the worker started still runs: `find /proc -maxdepth 2 -name cwd -lname '<worktree path>*'`, run from the base checkout. There it needs no exclusion, because the scan's own cwd is outside the worktree and it cannot match itself. An empty result shows an absence only beside a positive control: run the same command, with no exclusion, from a subshell `cd`'d into the worktree, where it lists the scan's own processes. A scan run from inside the worktree records its own subject, and `docs/procedure/anchor-a-claim.md` §Check the anchor, step 2 describes what such a basis needs. The one reading found no process left by a worker that had started none in the background.
2. **The worktree.** From the base checkout, run `git worktree remove <worktree path>`. Where `git worktree list --porcelain` reads `locked` for it, git asks for `--force` twice (§Host facts). Whether Claude Code ever removes the worktree or drops the lock itself is not measured, nor are the cause and timing of the lock's disappearance. Confirm with `git worktree list --porcelain`, which no longer lists it.
3. **The branches.** Ask the forge whether the work landed: `gh pr view <n> --json state,mergeCommit`, as its own command. On `MERGED`, delete the assigned branch with `git branch -D <branch>`. Also delete the host-named branch where `git branch --list worktree-agent-<agent id>` lists it and `git log main..worktree-agent-<agent id>` prints nothing. Confirm with `git branch --list <branch>` and `git ls-remote --heads origin <branch>`, both empty, for each branch.
