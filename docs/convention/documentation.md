# Documentation Conventions

What makes documentation valid in this project. Applies to every Markdown document in this repository, and to GitHub artifacts (issues, PRs, comments). Violations are grounds for PR rejection.

## Keep it maintainable

- **No ASCII art or text diagrams.** Boxes, arrows, and trees drawn with characters break on every edit. Express structure as bulleted lists of paths with one-sentence descriptions, or link to the code.
- **No implementation detail.** Code is the source of truth for how things work; docs record what exists and why. Do not transcribe function signatures, line-level behavior, or config dumps — they rot.
- **One file, one responsibility.** Split files by topic, not by length. A file that outgrows a single topic does not get a Contents list as a remedy — it gets split by responsibility, with each piece serving one topic.
- **Lists and tables over prose.** A rule scannable in 5 seconds beats a paragraph.

## Keep it current

- A PR that changes structure, workflow, or conventions updates the affected docs in the same PR. Stale docs are worse than missing docs.
- Delete docs that no longer describe anything real. Do not archive.
- Every claim about this repository must be verifiable in it. If you cannot point at it, remove it.
- A claim about anything outside it names what it holds for, precisely enough that a reader can check it there: a release where the project publishes them, a commit where it does not. `deprecated in v2` can be checked against the library and `deprecated` cannot; a path and a line number with neither looks precise and goes on looking valid after the line moves.
- A commit is cited by its full object name, never a prefix of it, when the citation is written; where a rule forbids editing a document, a citation already in it stays. A prefix that resolves today can become ambiguous as the repository grows, and nothing reports the day it does.
- A file may state once at the top what it was checked against, and then every claim in it holds for that. That statement records a check; it promises nothing about later versions. A reader past it holds claims nobody checked there, so moving the statement means re-checking the claims under it rather than editing the number, and a project receiving the statement over claims it has changed re-checks those claims or drops the statement.
- A citation of a rule numbered outside this project — in another project's document, or in this project's copy of one — quotes its heading or the rule's opening words, not its number, unless it stands in that same document, where whatever moves the number carries the citation with it. A renamed heading stops matching, so the citation breaks where a reader sees it; a reassigned number goes on resolving, to whatever rule took it. No heading in a file is a prefix of another heading in the same file: a citation of the longer goes on resolving against the shorter after the longer is deleted, so the break never reaches a reader. The number a whole document is filed under, an ADR's among them, is not a rule's: no edit to the rules inside a document moves it, so a citation keeps it.
- Where the claim is about a package's health rather than a release (`unmaintained`, `legacy`), do not rest a rule on it: name what to use instead, which does not expire.
- A claim about the project that will carry this document cannot be checked from here, so it is written as an obligation and not as a description. "`findOneByOrFail` throws, which the global filter maps" asserts a filter this file has never seen; "`findOneByOrFail` throws; map it in the filter at X" is a rule the reader can act on and a reviewer can look for.
- Update an existing document when the topic is already covered there; do not create a parallel document for a new facet of the same topic. Total document volume is a managed cost — a new file earns its place by adding a topic no existing file owns.

## GitHub artifacts

These are documentation too.

- **Facts only.** No rhetoric, no self-assessment, no inflated language ("perfect", "massive improvement"). State what changed, where, and why.
- **A claim names a basis that could have contradicted it.** Name what established it — the command with its arguments, the repository and revision of a file read, the source consulted — precisely enough that a reader can repeat it. A basis that would read the same had a part of the claim been false establishes nothing about that part: an expression matching nothing shows an absence only where the same expression is shown to match what is present, and a run whose output nobody has read is not a basis at all.
- **Stay inside the template.** No extra sections beyond the template fields; leave no field empty — write `N/A` with a reason. A line a tool appends to the body is outside the template and comes out, whichever tool appends it and whatever the line says; a project that keeps what such a line carries gives it a field in the template, which is the project's, rather than a line beneath the fields.
- **One comment, one point.** A comment carries a single request, instruction, or question. Ground it by citing a path or a rule, not by arguing.

## Format

- English and Markdown. The H1 takes the case of what it is — a name in title case, an exact file or repository name as spelled, a statement in sentence case (an ADR's H1 states its decision). Every heading below it is sentence case.
- Start each file with one line stating what it governs.
- A section whose territory abuts another document's states what it states — in its heading where that is shorter, in one opening sentence otherwise. The statement names the section's scope, not its rules.
- A `## Contents` list indexes a file's `##` headings, so how many of them there are decides whether it carries one and the file's length does not: more than eight, it opens with the list; three or fewer, it carries none; between the two, the writer decides.
- A file whose headings come from a template owes no Contents list, however many it has. Those headings are the same in every file written from that template, so listing them tells a reader only what the template already told them.
- **No decoration.** No emojis, badges, or ornamental headers. Nothing these rules reach is held to a looser standard: a check-mark glyph in a table is decoration where `yes` is a word.
- Use file paths (`src/auth/service.ts`) instead of drawn hierarchies.
- File names: lowercase kebab-case (`runtime-safety.md`). Uppercase is reserved for files a tool or a reader looks for by an exact name it did not choose: `README.md`, `AGENTS.md`, `CLAUDE.md`, and the names a tool looks for under `.github/`.
