# ADR 0035: The runtime API streams listings and searches and serves one alpha version at a time

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#246. List and Search of the runtime API answered with one message that held the whole result. The Guest Agent already sends both as streams of entries, 512 to a message, sorted by path, so mvmd gathered the stream into memory and sent it on as one message. A directory with enough entries made that message larger than a gRPC client takes by default (4 MiB), and the client refused it. Nothing on the daemon's side lifts a limit the client sets, so the result of such a directory could not be read at all.

The forces:

- Making a response a stream removes the unary form, which `runtime-api.md` §Scope and version counts as a change that needs a new version identifier.
- The version rule has no exception for the alpha stage, and the same section left open how many versions are served at once.
- The Search pattern had no syntax fixed, though the agent matches it as a glob, so a client could not know what it matched.
- The daemon is the node's runtime, and its memory must not grow with the size of a guest's directory.

Candidates weighed for the size of a result:

- **Stream it, sorted by path, and hold none of it in the daemon** — chosen. The guest's own contract is already a stream, so the daemon passes each message on as it arrives.
- **A stated bound on the result, refused when it is exceeded.** It gives every client an answer to "how large", and refuses a directory the guest has in full. The bound would be a figure the node picks for a guest's directory, and a client that needs the rest has no call to make.
- **Paging fields, a limit and a token, on the request and the response.** A client that does not send them gets the first page, and so a result silently cut short; the one that does needs a cursor that survives a change of the tree between pages, and the node would have to keep or recompute the sorted listing for each page.

Candidates weighed for the versions served:

- **Serve the current alpha version and stop serving the one before it** — chosen. Alpha is where a contract may still move, so serving only the current version leaves one contract for the daemon to implement and test, and the version number still tells a caller that something moved.
- **Keep `v1alpha1` served beside `v1alpha2`.** Every call of both would be kept working and tested, and the daemon would carry two generated packages and a mapping between them for a client that has no reason to stay behind.

Candidates weighed for the pattern of Search:

- **The Guest Agent's glob, its syntax stated in the contract** — chosen. It is what the agent matches today, so no behaviour changes, and the contract links to the definition.
- **A different syntax chosen by mvmd** (a substring, a regular expression). The daemon would translate it for the agent or filter the agent's answer itself, and would hold the result to do so.

## Decision

1. **List and Search are server-streaming and sorted by path across the messages.** The daemon passes each message of the guest on as it arrives and holds no whole result. The result has no bound set by mvmd and no paging field. A stream cut by a reset of the guest channel resumes after the last path sent.
2. **The runtime API is a new version, `v1alpha2`**, in the protobuf package `mvmd.runtime.v1alpha2` and the Go package `lib/runtimeapi/v1alpha2`, since the change removes the unary form.
3. **mvmd serves one alpha version at a time.** It serves `v1alpha2` and stops serving `v1alpha1`, and runtime info reports `v1alpha2` as the only version. A call to the service of another version is answered as unimplemented, which is gRPC's status for a service not hosted. This settles for alpha versions the open question of how many versions are served at once; the question stays open from `v1beta1` on.
4. **The `pattern` of Search is the Guest Agent's glob.** `runtime.proto` and `runtime-api.md` §Guest access state its syntax and link to where the agent defines it.

## Consequences

- A client reads a directory of any size as a stream and keeps what it wants. It no longer needs a larger receive limit.
- The daemon's memory for a listing is one message, not the directory.
- The Guest Agent still sorts the whole listing in memory before it sends the first message. That cost is the agent's and is not changed here.
- A client built against `v1alpha1` stops working with the daemon that serves `v1alpha2`, and moves to the new package, whose other calls are unchanged. There is no overlap in which both work.
- A change of the glob the agent matches is a change of this contract and needs a version under the same rule.
- A listing is a stream, so a call that fails after some messages ends with its outcome, and what was sent stays sent. A client that needs a whole result reads to the end and treats an outcome other than success as having none.
- This rules out a bound or paging on a result, a second served version of an alpha API, and a pattern syntax of mvmd's own.

## Signs the decision was wrong

- A client that needs a page or a count without reading the whole stream, because it does not want the whole directory, which would be the case for paging fields.
- A client held at `v1alpha1` that cannot move in step with the daemon, which would be the case for serving two alpha versions.
- A guest whose listing is large enough that the agent's sort in memory fails or stalls before the first message, which would move the cost the stream was meant to remove to the guest.
- A client that needs a different match than the glob gives, such as one that crosses `/` with `*`, which would be the case for a syntax of mvmd's own.
