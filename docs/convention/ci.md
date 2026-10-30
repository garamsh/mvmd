# Continuous Integration

How a project's checks are named, run, and secured. The rules hold on any toolchain; one toolchain appears at the end as a reference implementation.

## Contents
- Core principle
- One entry point per task
- Run the checks before pushing
- Reuse
- Verify a pinned dependency before adopting it
- Security baseline
- Reference implementation

## Core principle

Local checks and CI checks are the same checks, invoked by the same names. CI re-runs what a contributor already ran; it does not define a second way to run it.

## One entry point per task

This section states which names exist and what each owes, not the commands behind them.

- **One name per task.** Each of lint, format, test, and build has exactly one name that a human and CI both invoke, once the project has a command for it. What implements that name — a task runner, a script, a package manifest entry — is the project's choice; the name is the contract, so it arrives with the command and not ahead of it.
- **One name for the whole set.** A single name runs those names in order, so "run the checks" is one command locally and one step in CI.
- **A change shows that the whole set ran before it landed.** Once the project has a pipeline, every change that can land on the default branch carries, on the change itself, the record of a run of the whole set against it made before it lands, and what made that run: a pipeline run whose result is attached to the change, or a run outside the pipeline recorded there by whoever made it. A pipeline run that starts only after the change has landed, or only for changes bound for another branch, is no such record for it.
- **CI calls the names, not the commands behind them.** A pipeline that spells out the underlying tool invocations has created a second set of commands, and the two drift.
- **An entry point reports what it did.** A suite that skipped every test, a generator that found no input, a check that read only tracked files — each exits zero and reads exactly like the run that did the work. The output has to tell those apart: a count of what was covered, or a failure when the count is zero. A count taken over a set chosen for convenience is as honest as one taken over everything the task is meant to cover, and the two read the same, so the output names beside the count how the set it counted was selected.
- **An entry point may not become a pipeline component.** It runs on a contributor's machine as well as in CI.

## Run the checks before pushing

- Before code leaves the machine, run the cheap entry points: lint and format, and whatever the project folds into them. They catch the ordinary mistakes in seconds, so they are the first line of defence, and CI is not the only one.
- Where a pipeline run against the change being pushed will be the record §One entry point per task requires, test and build are the pipeline's to run, over the whole set. Where no pipeline run will be that change's record, they run locally before pushing too.
- Running more locally, the whole set included, is always allowed. This rule sets the least that runs before pushing, not the most.
- Automating the local run — a version-control hook, a watch task, an on-save editor command — is the contributor's choice. No tool is mandated, and running the checks by hand satisfies this section.

## Reuse

A pipeline step is one of two kinds. A check — lint, format, test, build — is invoked by its entry-point name (§One entry point per task). Everything else a pipeline needs — toolchain setup, caching, artifacts, deploys — is a step this section governs.

For each of those steps:

1. **Search for a maintained component** — a published action, plugin, or reusable job — at its current version, before writing pipeline code.
2. **Verify it** per the next section before pinning.
3. **Use it as its own documentation shows**. If no maintained component covers the step, a checked-in script is the fallback — not a license to hand-roll per-job.

## Verify a pinned dependency before adopting it

This section states what to establish before pinning and what makes a reference a pin, not which reference a given artifact takes.

Before pinning anything the pipeline pulls in — a published component, container image, or tool version — read its own documentation and determine:

1. **Current stable version** — not the remembered one. The version an agent recalls may be deprecated, archived, or replaced.
2. **Exact inputs that version accepts** — inputs shift between releases. Check them against the version's own documentation.
3. **Exact usage pattern** — follow what the documentation shows. Do not paraphrase inputs or rearrange the documented pattern.

Pre-trained recollection is a starting point, not source of truth. An agent that picks a version from memory silently uses a stale or nonexistent release.

Pin to a reference that cannot come to mean something else. A digest or a full commit SHA always satisfies that. So does a version where the ecosystem verifies content against a checksum log the repository also records — a Go module version under `go.sum` and the checksum database, where republishing a tag over different content fails verification instead of substituting silently; bypass that database and the property goes with it. A tag resolved through a mutable pointer, and any branch, satisfies it nowhere.

A pin fixes what a reference means, not what it points at: a digest never receives the patch its tag would have carried. So a pinned dependency also names how it gets moved — a bot that proposes the bump, a scheduled review, an owner — because pinning without that trades a moving dependency for a frozen one.

A reference no ecosystem can make immutable — a hosted runner label, a rolling base image, anything published without a digest — cannot be pinned, so neither "Pin to a reference that cannot come to mean something else" nor the obligation that a pinned dependency names how it gets moved reaches it. What it owes instead: the version it holds, named in the reference itself where the platform offers versioned forms; a mover, as a pin owes one; and a record, on each run, of what the reference actually resolved to. Fixing what a reference means in advance and knowing what it meant afterwards are the only two ways to read a failure that no change explains, and where the first is unavailable the second is not optional.

For every third-party dependency, also check the ecosystem's advisory source for known vulnerabilities in the candidate version.

## Security baseline

- **Minimal permissions**: the pipeline's default credential grants read only; grant write on the single job that needs it.
- **No plain-text secrets**: keep tokens, keys, and credentials in the platform's secret store. Prefer short-lived federated credentials (OIDC) over long-lived keys for cloud access.
- **Avoid script injection**: pass attacker-controlled input to a command through the environment, never interpolated into the command text.
- **Mask derived values**: values derived from secrets that can reach the logs are masked with the platform's masking mechanism.

## Reference implementation

The rules above bind. This section does not: it is one toolchain — Make for the entry points, GitHub Actions for the pipeline — that satisfies them. A project on another toolchain replaces this section and keeps every rule.

### Entry points as a Makefile

```makefile
.PHONY: lint format test build ci

lint:
	<lint-cmd>

format:
	<format-cmd>

test:
	<test-cmd>

build:
	<build-cmd>

ci: lint format test build
```

Replace `<lint-cmd>` and the rest with the project's actual commands (`npm run lint`, `ruff check`, `go test ./...`). Local `make lint` and CI `make lint` then invoke the same command.

### GitHub Actions specifics

- **Existing components**: search the GitHub Marketplace before writing inline `run:` blocks.
- **Advisories**: for every third-party action, check the GitHub Advisory Database (`github.com/advisories?query=type%3Areviewed+ecosystem%3Aactions`) for the candidate version.
- **Toolchain setup**: use the official setup action for the project's language; take its version and inputs from its own docs.
- **Checks**: run `make ci` — the target, not the commands inside it.
- **Artifacts**: use the artifact action.
- **Permissions**: `permissions: read-all` at the workflow top; `write` granted per job.
- **Secrets**: repo or environment secrets, OIDC for cloud deploys, `::add-mask::VALUE` for values derived from secrets.
- **Pinning**: a full commit SHA.
