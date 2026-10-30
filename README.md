# convention-driven-project

A GitHub template carrying the conventions a codebase is held to: how code, tests, commits, and reviews are written, how architecture decisions are recorded, and what a pull request or issue must contain.

The rules bind what a project writes, not who writes it (`docs/convention/README.md` §Scope, "These rules bind what the project writes"). The opening line of `docs/convention/README.md` addresses its reader as an agent, and `AGENTS.md` §Start here, every time sends every contributor, human or agent, to that file first. In `docs/convention/ci.md` §Verify a pinned dependency before adopting it, "Pre-trained recollection is a starting point, not source of truth" has nothing to fire on in a human team, while that section's obligations — reading the dependency's own documentation, pinning to a reference that cannot come to mean something else, naming how the pin gets moved — bind a human team as they bind an agent. `AGENTS.md` additionally states the contribution contract in the standard place an agent looks for it, the way `.editorconfig` states formatting in the place an editor looks.

## Usage

1. Create a repository with **Use this template**.
2. Configure its settings — §Configure the repository, below.
3. Bootstrap it — §Bootstrap, below.

To bring these conventions into a repository that already exists, copy the parts it will enforce and then bootstrap the same way.

This file is replaced during bootstrap; do not edit it to describe your project. The procedure below sits here rather than under `docs/` for that reason — a project created from the template does not inherit instructions it has already outgrown.

## Configure the repository

Configure and verify the receiving repository's settings:

- **Branch protection on `main`** — require pull requests and block direct pushes. Configure protection so changes reach `main` through a pull request. Keep one designated merge authority, enforced by convention where repository settings cannot restrict who merges.
- **Require review before merge** — only if the reviewer uses a separate account. A pull request's author cannot approve it (`docs/convention/review.md` §Single-account setups names the basis), so with one shared account this blocks every merge; leave it off and the merge itself serves as the approval.
- **Squash-only merges** and **automatically delete head branches** — match `docs/convention/git.md` and keep branches from piling up.
- **Require status checks** once CI exists, so the project's single entry point for the whole check set gates merges (`docs/convention/ci.md`).
- **Labels `task` and `proposal`** — the shipped issue templates declare them (`.github/ISSUE_TEMPLATE/task.md`, `.github/ISSUE_TEMPLATE/proposal.md`); ensure both labels exist before accepting issues from those templates.

A setting the platform puts out of reach — a plan that gates it, an organisation policy, a forge that has no such thing — does not release the rule it would have enforced. That rule holds the way the single merge authority does above: by convention, with the review the only thing standing between a change and `main`.

## Bootstrap

Run in order. Every step touches only paths in this repository. Bootstrap is a change like any other: it runs on a branch and lands as a pull request (`docs/convention/git.md`).

1. **Pick the conventions that apply.** Under `docs/convention/`, keep the `stack-*.md` files matching the project's stack, plus the base of any kept file the conventions index gives one; delete the rest. Keeping none is a valid outcome — a project no stack file covers runs on the stack-neutral files alone. Done when the `stack-*.md` files that remain are exactly those naming a stack the project uses, plus the base that index gives any of them.
2. **Extend `.gitignore`.** Add the build outputs, dependency directories, and tool caches of the kept stacks to the last block of `.gitignore`.
3. **Set up CI plumbing.** The template ships no entry points and no pipeline. When the project has a command for a task, add the entry-point name `docs/convention/ci.md` §One entry point per task requires for it, the command behind it, and a pipeline that calls the names; that file's §Reference implementation is one way to do it. Where the pipeline keeps a reference no ecosystem can make immutable — a hosted runner label is one — name its mover, as `docs/convention/ci.md` §Verify a pinned dependency before adopting it, in the paragraph beginning "A reference no ecosystem can make immutable", owes it. The project supplies it because a template has no owner to name. Done when every bullet of `docs/convention/ci.md` §One entry point per task holds of the entry points and the pipeline the project has, and each reference the pipeline keeps that no ecosystem can make immutable has a mover the project can point at.
4. **Choose the system's structure.** Read the candidates under `docs/architecture/structures/` side by side and take one. Record the choice as an ADR, carrying that file's `docs/architecture/structures/` §Signs the choice was wrong into the ADR's `docs/architecture/adr/0000-template.md` §Signs the decision was wrong, and its `docs/architecture/structures/` §Boundaries and dependency direction together with `docs/architecture/README.md` §Shape rules every project carries into `docs/architecture/structure.md` (`docs/architecture/README.md` rules 6 and 7). Delete `docs/architecture/structures/` and that section only once both exist — they are what the two are written from. A project that writes no `structure.md` has nothing to write from them: it copies nothing and deletes both without waiting. Done when `docs/architecture/structures/` and that section are gone and, where `structure.md` exists, an ADR names the shape taken and carries every bullet of that shape's `docs/architecture/structures/` §Signs the choice was wrong, and `structure.md` carries every bullet of its `docs/architecture/structures/` §Boundaries and dependency direction and of `docs/architecture/README.md` §Shape rules every project carries.
5. **Write the architecture documents.** Add the responsibility documents `docs/architecture/README.md` requires for the domains the project already has, list every responsibility document and ADR in its §Index, and leave `docs/architecture/adr/0000-template.md` in place. Done when every domain the project has carries a responsibility document, `docs/architecture/README.md` §Index lists every responsibility document and ADR the repository holds, and the template is still there.
6. **Refresh the conventions index.** Update the stack-specific section of `docs/convention/README.md` to name the `stack-*.md` files that remain.
7. **Replace the CODEOWNERS placeholder.** Replace `@project-owner-placeholder` in `.github/CODEOWNERS` with the project owner.
8. **Replace this README.** Describe the project instead of the template. `AGENTS.md` stays as it is.

## Structure

- `AGENTS.md` — the contribution contract: who may change what, and the rules that apply to every contributor
- `docs/convention/` — conventions for code, reviews, and documentation; `stack-*.md` files are pruned during bootstrap
- `docs/architecture/` — responsibility documents (current truth), ADRs (append-only decision log), and the candidate structures bootstrap chooses between and then deletes
- `docs/procedure/` — this repository's own procedures for its recurring acts, not part of what a project takes; a project wanting the layer writes its own. Because the layer stays here, a procedure may name the host its acts run on and the version of that host's tool it was read from, as `docs/procedure/dispatch-a-worker.md` does; a convention file travels to every project that takes it, where that host need not exist
- `.github/` — PR and issue templates, and CODEOWNERS
