# Tailwind CSS

Defaults for Tailwind CSS projects. How styles are composed: utilities in markup first, with `@apply`, `@utility`, and theme tokens only where they earn their place.

Checked against Tailwind CSS v4.3.3 — `tailwindcss`, `@tailwindcss/cli`, `@tailwindcss/postcss`, `@tailwindcss/vite` and `@tailwindcss/upgrade`, all at that release. A claim below that names no version holds for it.

## Contents
- Core principle
- Verify before adopting or upgrading
- Tailwind v4 specifics
- Composition: inline utilities first
- `@apply` and `@utility`: when each earns its keep
- Theme tokens via `@theme`
- Common idioms

## Core principle

Compose simple utilities in markup. The cheapest style is the one the framework already ships. Custom CSS is only justified when a *specific* combination repeats across many sites and no single utility captures it.

## Verify before adopting or upgrading

Tailwind's own locators for a version check: `tailwindcss.com/docs/installation`, `tailwindcss.com/docs/upgrade-guide`.

## Tailwind v4 specifics

- Install: `npm install tailwindcss @tailwindcss/vite` (Vite) or `@tailwindcss/postcss` (PostCSS). CLI lives in `@tailwindcss/cli`.
- Import: `@import "tailwindcss";` in your CSS. **No `@tailwind base/components/utilities` directives** — v4 compiles them without an error, but they load no theme and no preflight, so `p-4` or `bg-red-500` generates nothing.
- Config is CSS-first via `@theme { ... }` blocks in your stylesheet. **No `tailwind.config.js` required.**
- Browser target: Safari 16.4+, Chrome 111+, Firefox 128+. For older browsers, stay on v3.4.
- Custom utilities: `@utility <name> { ... }` (replaces `@layer utilities` from v3).
- v3 config features, each with its own fate:
  - `corePlugins` — not supported. Under `@config` it is ignored without a warning.
  - `safelist` — not supported, ignored the same way. Use `@source inline("...")`.
  - `resolveConfig` — removed. Read theme values as CSS variables.
  - `theme()` — still works, deprecated. Use `var(--color-red-500)`; where a variable cannot go (a media query), `theme(--breakpoint-xl)`.
- The upgrade tool (`npx @tailwindcss/upgrade`) handles most migrations.
- Native CSS nesting supported — no preprocessor step needed.

## Composition: inline utilities first

Default to composing utilities directly in markup. Order is irrelevant; output is deterministic.

```html
<button class="inline-flex items-center gap-2 px-4 py-2 rounded-md
               bg-blue-500 text-white font-medium
               hover:bg-blue-600 active:bg-blue-700
               focus:outline-2 focus:outline-offset-2 focus:outline-blue-500
               disabled:opacity-50 disabled:cursor-not-allowed">
  Submit
</button>
```

Inline utilities make components transparent: a reviewer reads the markup and sees every style applied, with no layer of indirection.

## `@apply` and `@utility`: when each earns its keep

Decision order:

1. **Inline utilities** — one-off layouts and styles. Default.
2. **`@apply`** — when the same 5+ utility chain appears in 3+ components. Pull the chain into a class via `@apply` and let the build resolve it.
3. **`@utility <name> { ... }`** — when you need a *new* class that responds to variants (`hover:`, `focus:`, `lg:`) Tailwind doesn't ship.

Avoid `@apply` for 1–3 utilities — inline them. `@apply` chains longer than ~10 utilities usually signal the wrong abstraction; the design likely needs a token or a component, not more classes.

Rung 2 — the chain repeats, so it becomes a class:

```css
.btn-primary {
  @apply inline-flex items-center gap-2 px-4 py-2 rounded-md
         bg-blue-500 text-white font-medium
         hover:bg-blue-600 focus:outline-2 focus:outline-blue-500;
}
```

Rung 3 — a class Tailwind does not ship, left open so variants can
target it (`md:content-auto`, `lg:content-auto`):

```css
@utility content-auto {
  content-visibility: auto;
}
```

Do not write the variants into a `@utility` body. A rung-3 class
exists so the caller can apply them; baking them in is rung 2 with
the wrong keyword.

## Theme tokens via `@theme`

Define design tokens once. Components reference tokens, never raw values.

```css
@theme {
  --color-brand-500: oklch(0.65 0.18 250);
  --font-display: "Inter", "system-ui", sans-serif;
  --radius-card: 0.75rem;
}
```

Use as `bg-brand-500`, `font-display`, `rounded-card`. Hardcoded hex or px in markup is a smell — extract it. Tailwind's own `--tw-*` variables sit behind that surface and are an implementation detail; reference your tokens, never those.

## Common idioms

- **Responsive** — mobile-first: `md:grid-cols-2 lg:grid-cols-3`.
- **State** — `hover:`, `focus:`, `active:`, `disabled:`, `group-hover:`, `peer-*`.
- **Dark mode** — `dark:bg-gray-900`. It follows `prefers-color-scheme` by default; toggling via `<html class="dark">` needs `@custom-variant dark (&:where(.dark, .dark *));`.
- **Layout** — prefer flex/grid + `gap-*` over `space-x-*` / `space-y-*` (v4 selector change for performance).
- **Sizing** — `size-*` for square; `w-*`/`h-*` otherwise.
- **Color with opacity** — `bg-black/50` (slash syntax). v3's `bg-opacity-*` is removed.
- **Arbitrary values** — `bg-[#ff00aa]` or `bg-(--brand-color)` for CSS vars. Search the docs first; a utility usually exists.

