# Next.js (App Router) — Architecture & Style Conventions

How a Next.js project is laid out and where each kind of code
belongs: routes, the server/client boundary, mutations, env, and
tests.

Checked against Next.js 16.4.0, create-next-app 16.4.0,
`@next/codemod` 16.4.0, `server-only` 0.0.1, shadcn 4.21.4, and
Vitest 5.0.3. A claim below that names no version holds for these.

## Contents
- 0. Folder & file naming
- 1. Project layout (App Router, `src/`)
- 2. App Router file conventions
- 3. Server Component vs Server Action vs Route Handler
- 4. Server Components — pages and layouts
- 5. Server Actions
- 6. Route Handlers — external use only
- 7. Errors — returned, thrown, or caught
- 8. Env (single typed loader at project root)
- 9. Multi-layout via route groups
- 10. Parallel routes and intercepts (modal pattern)
- 11. `proxy.ts` (v16+) / `middleware.ts`
- 12. Testing — three layers, two locations
- 13. Feature entry points and file naming

## 0. Folder & file naming

The names the official tooling ships are the canonical ones: if a
name appears in the Next.js docs or in a `create-next-app` default,
adopt it.

- Official, and kept as shipped: `app/` (routes), `public/` (static
  assets), `src/` — optional in Next.js, required here (§1) — and
  the App Router file conventions
  (`layout.tsx`, `page.tsx`, `loading.tsx`, `error.tsx`, `route.ts`,
  `template.tsx`, `default.tsx`, `proxy.ts` / `middleware.ts`).
- Files named for a concept: `src/lib/format-currency.ts`,
  `src/lib/email-validation.ts`.

## 1. Project layout (App Router, `src/`)

Use the `src/` layout. `src/app/` holds routes and nothing else.

At the project root:

- `src/` — all application source. Its contents are listed below.
- `public/` — static assets, at the root, never inside `src/`.
- `e2e/` — Playwright E2E specs, at the root, never inside `src/`.
- `tsconfig.json` — maps the `@/*` path alias to `./src/*`.
- `next.config.ts`, `package.json`, `eslint.config.mjs`,
  `playwright.config.ts`, `vitest.config.ts` — project config.
- `.env.example` and `.env.local` (gitignored) — env files stay
  here, at the root (§8).

Under `src/`:

- `src/proxy.ts` — the v16 request proxy (§11).
- `src/app/` — routes and the App Router file conventions of §2,
  and nothing else.
- `src/app/api/` — Route Handlers, for external callers only (§6).
- `src/app/<segment>/_components/` — components belonging to one
  route, kept beside it; the `_` prefix opts the folder out of
  routing.
- `src/features/<name>/` — a vertical slice, owning its own
  `api/`, `components/`, `hooks/`, `types/` — only the ones the
  feature needs, not a fixed set per feature.
- `src/components/` — shared UI; `src/components/ui/` is the
  shadcn primitives home.
- `src/hooks/` — shared React hooks.
- `src/lib/<vendor>.ts` — pre-configured third-party clients and
  small shared helpers: `auth.ts`, `db.ts`, `api-client.ts`,
  `utils.ts` (the shadcn companion), `rate-limit.ts`, …. A
  vendor that has outgrown one file gets `src/lib/<vendor>/`.
- `src/env.ts` — the typed env loader (§8).
- `src/testing/` — test infrastructure: render helper, MSW
  handlers, MSW server (§12).
- `src/types/` — types extracted below the features.
- `src/styles/globals.css` — global styles.
- `src/instrumentation.ts` — optional OpenTelemetry hook.

A cross-cutting third-party client belongs at `src/lib/<vendor>.ts`,
or at `src/lib/<vendor>/` once the adapter outgrows one file.

## 2. App Router file conventions

A route is **not publicly accessible** until a `page.tsx` or
`route.ts` exists in the segment.

Never mix the App Router and the Pages Router in one project: no
`pages/` directory beside `app/`. A project runs one router.

| File | Role | Key constraint |
|---|---|---|
| `layout.tsx` | Shared UI for segment + descendants. State preserved. | Root **must** render `<html>` and `<body>`. |
| `page.tsx` | Route UI; leaf of subtree. Server Component by default; may be `async`. | Required for segment to be public. |
| `loading.tsx` | Suspense fallback. | Cannot show when layout reads uncached data — wrap in `<Suspense>` or move to `page`. |
| `error.tsx` | Error boundary. | **Must be a Client Component** (`'use client'`). |
| `global-error.tsx` | Replaces root layout on catastrophic error. | Must include own `<html>` and `<body>`. |
| `not-found.tsx` | UI for `notFound()` and unmatched URLs. | Streamed → 200, non-streamed → 404. |
| `route.ts` | HTTP request handler. Exports `GET`/`POST`/`PUT`/`PATCH`/`DELETE`/`HEAD`/`OPTIONS`. | Web `Request`/`Response` only. No JSX. |
| `template.tsx` | Like layout but re-mounts on navigation. | Use sparingly. |
| `default.tsx` | Fallback for unmatched parallel route slots. | Required when using parallel routes. |

### Folder conventions

| Syntax | URL effect | Use |
|---|---|---|
| `folder/` | adds URL segment | Standard route |
| `[param]/` | dynamic | `params.param` |
| `[...rest]/` | catch-all | `params.rest: string[]` |
| `[[...rest]]/` | optional catch-all | `params.rest?: string[]` |
| `(group)/` | **no** URL effect | Organization, multiple root layouts |
| `_folder/` | **no** URL effect | Private — opted out of routing |
| `@slot/` | **no** URL effect | Named slot for parallel routes |
| `(.)folder`, `(..)folder`, … | intercepts route | Modals |

Route group rules: routes in different groups at the same segment
**must not** resolve to the same URL. Navigating across **multiple
root layouts** causes a full reload. `/` must live in one group when
using multiple root layouts.

## 3. Server Component vs Server Action vs Route Handler

| Concern | Server Component | Server Action | Route Handler |
|---|---|---|---|
| Read data | yes, async render | — | yes, in handler |
| Mutations from forms | — | yes, **default** | no (§6) |
| Mutations from `onClick` | — | yes, via `useTransition` | no (§6) |
| Webhooks from third parties | — | no | **only choice** |
| Public REST/JSON API | — | no | **only choice** |
| OAuth callbacks | — | no | yes |
| Streaming (ReadableStream) | partial, via Suspense | — | yes |
| Works without JS | yes | yes | no |

### Decision rule

1. **External HTTP API / webhook** → `src/app/api/<path>/route.ts`
2. **Form submission / mutation from your own UI** → Server Action
3. **`onClick` mutation with optimistic UI** → Server Action via
   `useTransition` / `useActionState`
4. **Read data for a page** → Server Component (async, fetch inline)
5. **React to a mutation** → Server Action calls `revalidatePath` /
   `revalidateTag`, `redirect()`, or `refresh()` from `next/cache`

### `'use client'` placement

Add to **specific interactive components**, not layouts or whole
pages. Once a file is `'use client'`, all its imports become
client-bundled. Use `import 'server-only'` for modules that must
never reach the client. Non-`NEXT_PUBLIC_` env vars are not inlined
into the client bundle and read as `undefined` in the browser, but a
Client Component's server render reads the real value. The prefix
keeps a value out of the bundle, not out of the page: a secret read
in a `'use client'` module reaches the HTML.

## 4. Server Components — pages and layouts

- Pages are `async` Server Components that fetch their data inline:
  no `'use client'`, no `useEffect`.
- `params` and `searchParams` are **promises** in v15+ — `await`
  them.
- Layouts **do not re-render** on navigation, so they receive no
  `searchParams`. Read live values in a Client Component.
- `loading.tsx` shows no fallback for a layout reading uncached data
  (`cookies()`, `headers()`, an uncached `fetch`). Without
  `cacheComponents` navigation blocks on it; with it, which
  create-next-app writes into `next.config.ts`, `next build` fails.
  Wrap the read in `<Suspense>` or move it to `page.tsx`.

## 5. Server Actions

A Server Action carries the `'use server'` directive at the top of
its module, and is the default for mutations driven by your own UI
(§3). Its `FormData` argument reaches it over the network, like any
other request body.

**Critical security:** Server Functions POST to the route where
they're used. A `proxy.ts` matcher that excludes that path silently
disables auth on its Server Actions. **Always re-verify the session
inside the Server Action.**

## 6. Route Handlers — external use only

- Route Handlers serve external callers: webhooks, OAuth callbacks,
  public REST/JSON endpoints, streamed responses. **Never** use one
  for a mutation your own UI submits — that is a Server Action (§3).
- A webhook handler verifies the provider's signature against the
  raw request body before acting on the event, and answers 400 when
  the signature is missing or fails. A body already parsed as JSON
  can no longer be verified.

## 7. Errors — returned, thrown, or caught

- **Expected failure is a return value.** A rejected password or a
  taken email comes back from the Server Action as data, and
  `useActionState` renders it. Throwing puts `error.tsx` over the
  segment instead, which loses the form and everything typed into it.
- **A bug is a throw**, and the nearest `error.tsx` catches it. A
  route whose subject does not exist is neither: that is
  `notFound()`, which renders `not-found.tsx`.
- **A thrown error's message does not reach the browser.** In
  production an error thrown on the server arrives at `error.tsx` as
  a generic message and a `digest` hash matching the server log. What
  the user must read is returned; what you must read is logged.
- **The error you return crosses the wire.** Return the sentence the
  UI renders, not the failure you caught: an action's return value is
  serialized to the client, so whatever detail rides on it goes too.
- **`error.tsx` does not cover the `layout.tsx` beside it**, only the
  segment's page and what nests below. A layout that throws is caught
  one segment up, and the root layout only by `global-error.tsx`.
- **`redirect()` throws to do its job.** Call it outside `try`, or
  the `catch` takes the navigation for a failure. Nothing after it
  runs, so `revalidatePath` / `updateTag` come first when the
  destination reads what the action just wrote.
- **An error boundary catches renders, not handlers.** A throw inside
  an `onClick` reaches no boundary and the interaction stops with
  nothing shown. Run it through `startTransition`, whose throws do
  bubble, or catch it and put it in state.

## 8. Env (single typed loader at project root)

- One typed, validated loader, at `src/env.ts`. Everything else does
  `import { env } from '@/env'`. **Never** scatter `process.env.X`
  reads throughout the code.
- Env files stay at the project root. Load order, first match wins:
  `process.env` → `.env.$(NODE_ENV).local` → `.env.local` (not in
  test) → `.env.$(NODE_ENV)` → `.env`.

### Browser exposure

- Without `NEXT_PUBLIC_` prefix → server only.
- With `NEXT_PUBLIC_` prefix → inlined into the client bundle at
  build time; **frozen at build** — changing it after `next build`
  has no effect.
- A lookup whose key is known only at runtime (`process.env[varName]`)
  is NOT inlined.

## 9. Multi-layout via route groups

- Each route group owns its layout: `(marketing)/layout.tsx` for the
  public section, `(app)/layout.tsx` for the authenticated one,
  `(auth)/layout.tsx` for the login and registration flows. The root
  `src/app/layout.tsx` renders `<html>`/`<body>`, fonts, and
  providers.
- **Keep multiple root layouts rare** — navigation between them is a
  full reload.

## 10. Parallel routes and intercepts (modal pattern)

For modals that stay shareable, refresh-safe, and clean under
back/forward. The pieces, for a photo modal:

- `src/app/@modal/default.tsx` — fallback for the unmatched slot.
- `src/app/@modal/(.)photos/[id]/page.tsx` — the intercepted route,
  rendered as the modal. `(.)`, not `(..)`: a slot is not a route
  segment, so `photos` is on the same level.
- `src/app/photos/[id]/page.tsx` — the real page, rendered on direct
  navigation.
- `src/app/layout.tsx` — renders both `{children}` and `{modal}`.

## 11. `proxy.ts` (v16+) / `middleware.ts`

- The file lives at `src/proxy.ts` (§1). It exports a `proxy`
  function and a
  `config` object whose `matcher` selects the paths it runs on —
  conventionally everything except `/api`, `_next/static`,
  `_next/image`, and `favicon.ico`.
- v16 renamed `middleware.ts` → `proxy.ts`. Codemod:
  `npx @next/codemod@16.4.0 middleware-to-proxy .`.
- Use cases: auth gate (redirect), geolocation, A/B headers.
- **Anti-patterns:** don't use as a general middleware hub; don't
  rely on it for Server Action security — matchers can exclude
  paths silently (§5).

## 12. Testing — three layers, two locations

| Layer | Tool | Location |
|---|---|---|
| Unit | Vitest + Testing Library | `src/**/__tests__/*.test.ts` |
| Integration | Vitest + Testing Library + MSW | `src/features/<f>/__tests__/*.test.ts` or `src/app/<r>/__tests__/*.test.ts` |
| E2E | Playwright | `e2e/*.spec.ts` |

- Test names: `describe(...) > it(...)`.
- The in-process client is Testing Library's `render()` for a
  component and route segment fetching for a whole route.
- MSW handlers live at `src/testing/mocks/handlers.ts`.
- The render helper lives at `src/testing/render.tsx`. It carries
  what every test would otherwise repeat: awaiting an async Server
  Component before handing the resolved tree to Testing Library's
  `render()`, and wrapping whatever providers the root layout mounts.
  A provider mounted over one segment is wrapped by the tests of that
  segment, not here.
- Nothing outside a test imports `src/testing/`.
- The test setup stubs `server-only` to an empty module. That package
  throws wherever it resolves without the `react-server` export
  condition — in a Client Component, and in a Vitest run — so
  without the stub every test reaching a server-only module fails
  before it runs.
- This stack's substitutes: MSW for outbound HTTP, a Prisma/Drizzle
  test double or SQLite for the database, a fake session for auth,
  `testcontainers` for real service dependencies.
- The built app is `next start`.
- Assert on rendered text, ARIA, emitted form payloads, and
  navigated URLs — **never** on the internal React tree shape or
  component identity.
- **A text assertion is identity or presence, and the two are not
  interchangeable.** Where the accessible name *is* the string — a
  heading, a button — assert the whole name: a substring match still
  passes when the UI appends to the text, so it cannot catch the
  change. Where the name carries more than the string — a row, a
  card — a substring match is right, and the call site says so in a
  comment. Which query spells which differs by tool, and the two
  default opposite ways — checked against `@testing-library/dom` 10.4
  and Playwright 1.63:

| Tool | Identity | Presence |
|---|---|---|
| Testing Library | `getByRole('heading', { name: 'Cart' })` — a string `name` is compared against the whole accessible name, so the string alone is the identity assertion and `ByRoleOptions` carries no `exact` to add | `getByRole('heading', { name: /Cart/ })` — an unanchored `RegExp` is a substring test |
| Playwright | `getByRole('heading', { name: 'Cart', exact: true })` | `getByRole('heading', { name: 'Cart' })` — a bare string matches a case-insensitive substring |


## 13. Feature entry points and file naming

A feature declares two entry points, not one:

- `features/<name>/index.ts` — what a Server Component may read.
  Server-only modules reach the graph through here.
- `features/<name>/actions.ts` — the feature's Server Actions. A
  client module imports from here.

One barrel over both breaks the build. A barrel that exports anything
client-reachable is treated as client-reachable whole, so a client
module importing a single action drags the server-only modules beside
it into the client graph. The error is then reported at the
server-only module, with the client module only in its import trace —
the shape of the barrel is the cause.

### File naming

- Component: `<name>.tsx` (multi-word: `theme-toggle.tsx`).
- Hook: `use-<name>.ts`.
- Server Action: `<verb>-<noun>.ts` with `'use server'`.
- Test: `*.test.ts(x)` inside `__tests__/`.

