# Style Conventions

Consistency in how code is written. A formatter owns layout; these rules own what formatters cannot judge.

## Naming consistency

These rules state which names must agree, not their concrete form: casing, affixes, a file name's spelling, or the names a stack bans.

- **Same concept, same name.** Pick one term per domain concept and use it everywhere. Mixing `user` and `member` for one thing is a defect, not a choice.
- **Same kind, same shape.** Functions doing similar work follow the same verb pattern (`getUser`, `getOrder` — not `getUser`, `fetchOrder`).
- **Shared concept, named file.** When two helpers share a concept, give the concept a name and name their file for it. The path then says what the file does, not that it holds helpers.

## Pattern consistency

- **Same problem, same solution shape.** When an earlier module solved a problem one way, later modules solve it that way too. Introduce a new pattern only when the existing one demonstrably cannot solve the case — and say so in the PR description.
- **Copy the local idiom.** New code mimics the conventions of the module it lands in.

## Priority

Module's existing conventions > project conventions > personal taste. "My way is better" is never a valid reason for divergence; propose the change instead.

## Boundaries

- No style-only diffs. Restyling existing code to your taste is a scope violation; changing the established style project-wide is a `proposal` issue, not a drive-by edit.
