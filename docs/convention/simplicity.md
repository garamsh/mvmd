# Simplicity Conventions

The default response to a problem is to change the shape, not to add to it.

## Core principle

Where a defect or a requirement can be met either by adding — a branch, a guard, a special case, a wrapper, a file — or by changing the shape so the case cannot arise, take the second. Adding is correct once the restructure has been considered and rejected, and the pull request says why.

Every line, file, and folder is one more the next reader holds and the next change keeps working. That cost is what this rule is paid to avoid. Additions collecting in one place are worth reading as a sign the shape is wrong.

## Rules

These rules state what earns a division or an abstraction, not the count or size that marks the point.

- **A division is a response, not a plan.** Start flat. Split a file, or introduce a folder, when the current shape already holds more than one thing — never because a division is expected later. An empty container is a claim the code does not support.
- **No speculative abstraction.** Do not build for imagined future needs: no configuration options nobody asked for, no indirection layers for one caller. Add the abstraction when the second real case arrives — except a seam, a boundary declared so that a test or an alternative implementation can take the place of the real thing, which earns its keep on the first.
- **An abstraction that hides nothing is complexity.** One standing between a caller and a single concrete thing, adding no meaning and no seam, is removed.
- **Consolidate with judgment.** When two features look similar: first verify whether they truly differ. If a unified module is more intuitive and does not need flags and conditionals everywhere, unify them. If unification requires branching at every turn, they are different in essence — keep them separate. A wrong abstraction is worse than honest duplication.
- **Delete, don't wrap.** Prefer deleting dead code and dead paths over layering compatibility shims on top of them.
- **Structural complexity is reviewable.** Nesting depth, indirection hops, and the number of concepts a reader must hold at once are all reviewable. A unit harder to read than the problem it solves is restructured before shipping.
