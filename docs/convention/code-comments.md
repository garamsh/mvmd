# Code Comment Rules

When and how to comment code — and when to leave it alone.

## When to comment

Add a comment where it helps a reader. Skip where the code is self-evident. When in doubt, prefer to omit; an absent comment is easier to add later than an absent explanation is to remember.

A comment that no longer describes the code below it is corrected or deleted in the same change that moved the code. A stale comment is worse than an absent one: it is read as true.

## Brevity

When you do comment, write the shortest version that conveys the essential point. One short sentence beats a paragraph.

Long comments drift out of sync with the code they explain; short ones age well. If a comment needs more than 2–3 sentences, refactor the code first — extract the explanation into a doc comment on a named function or type where readers can find it.

## Voice

Write comments in declarative or imperative mood, stating facts: `returns the cached token`, `retries once on timeout`. Never narrate ("this will now…", or `// loop through users` above a for-loop), apologize ("unfortunately we have to…"), or justify the author's choices ("I chose this because…") — rationale belongs in the commit message or an ADR.

## Inline vs doc comments

This section states where each kind of comment earns its place, not its style or tooling.

**Inline** (inside function bodies) — earn their place when they flag a non-obvious invariant, an ordering constraint, or an external constraint a future reader would otherwise re-derive incorrectly.

**Doc comments** (above types, functions, modules) — required for any public API exposed to other humans or modules.

## Forbidden

- **Paraphrasing** (`// increment counter` above `i++`)
- **Section dividers** (`// constructor` / `// getters` / `// setters`)
- **Wall-of-text** above a function — one line at the top of the body, or a doc comment on the type/module, is enough

## TODOs

`// TODO(name): …` with an owner. One marker, so one search finds every open item — never `FIXME` or `XXX` beside it.

## Language

Default to English. Override when the team, project, or audience makes a non-English language the more maintainable choice (e.g. internal corporate code in a non-English-speaking org). Open-source and cross-team code: English.