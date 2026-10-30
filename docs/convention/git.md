# Git Conventions

How branches, commits, and PR titles are written, and what survives a merge.

## Branches

- `feat/<short-name>` — new behavior
- `fix/<short-name>` — bug fixes
- `chore/<short-name>` — tooling, config, dependencies
- `docs/<short-name>` — documentation only
- `refactor/<short-name>` — behavior-preserving restructure
- `test/<short-name>` — test-only changes

One task, one branch. Branch from the default branch; never commit to it directly.

## Commits

A squash merge puts the PR title on the default branch and drops the authors of the commits behind it. Whoever merges ensures the commit that reaches the default branch carries a `Co-authored-by:` trailer for each of them, and confirms it by reading that commit against the commits it squashed rather than assuming the merge carried them over. A branch's commit messages are otherwise a working record for the reviewer reading the branch, not the permanent history: a line a tool appended to a branch commit is neither required there nor to be removed.

An author is a person. An agent that wrote a change is the instrument of its author, not a second author. The record that an agent wrote a change is its pull request, so the commit that reaches the default branch names that pull request in its subject.

Format: `<type>: <imperative summary>`

- Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`
- Summary: imperative mood, lowercase, no trailing period. `fix: reject empty tokens`, not `fixed some bugs`.
- One concern per commit. If a change needs "and" to describe it, split it.
- Omit the body unless the reason for the change is invisible in the diff. A body never restates what changed.
- The commit that reaches the default branch contains only what the project put there — no line a tool appended unasked, whichever tool appended it and whatever the line says. A `Co-authored-by:` naming an author of the squashed commits is not such a line: the project asks for it.
- Reversing an earlier decision mid-branch is an ordinary commit describing the new state. Earlier commits are not rewritten to hide that the decision changed.
- A point about a commit message is fixed in the commit that carries it — amend or rebase, then force-push the same branch. A later commit cannot remove text from an earlier one, so it cannot carry that fix. This is the one case where rewriting a branch commit is right.

## PRs

- Title follows the commit format: `<type>: <imperative summary>`. It becomes the commit on the default branch, so it is the permanent record of the change.
- A change to a file other projects copy from this one that renames, renumbers, or removes a heading or a numbered rule names the old one in its pull request title. That title is what reaches a project taking the change, and nothing else tells it which of its citations to re-check.
- Body follows `.github/PULL_REQUEST_TEMPLATE.md`.
- A promotion is a PR from the default branch into a branch that only promotions move. It is merged by fast-forward, never by a squash, a rebase or a merge commit: each of those leaves that branch holding a commit the default branch never held. Its title becomes no commit; the commits it carries keep the titles and trailers their own squash merges gave them.
- PRs other than a promotion are squash-merged. Branch commits do not appear on the default branch; do not rewrite them to be pretty.
