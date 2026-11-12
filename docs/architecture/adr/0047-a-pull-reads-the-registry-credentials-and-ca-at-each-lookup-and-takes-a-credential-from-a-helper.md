# ADR 0047: A pull reads the registry credentials and CA at each lookup, and takes a credential from a helper

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#434, for the criterion "a registry credential that expires is renewed without restarting the daemon" of garamsh/aurora#749. The registry credentials were read once at start from `registry.credentials_file` ([image.md](../image.md) §Pull image, before this ADR), so a token written into the file later was never seen, and an ECR token lasts 12 hours. The node also had no way to trust the CA of a private registry.

Candidates weighed for renewal:

- **Watch the file and reload.** A watcher is a second mechanism with its own failure cases (missed events, partial writes), and a read at the lookup needs none.
- **Re-read the file at each credential lookup of a pull** - chosen for the file. Whoever rewrites the file renews the credential.
- **Run a docker credential helper at each lookup** - chosen for a registry whose credential mvmd should not hold in a file. For ECR the helper is `ecr-login` (amazon-ecr-credential-helper); shipping it and its AWS identity is the node's, not mvmd's.
- **Keep read-once beside the new forms.** Rejected: a mode that keeps the old behaviour is a second path to maintain for no caller.

## Decision

1. **`registry.credentials_file` is read at each credential lookup of a pull**, with the checks it had at start: absolute path, regular file, closed to group and others, valid JSON, a username and a password per entry. A read that fails fails that pull with the reason and never prints a value. Start still reads it once and refuses a file that fails, so a bad node fails early. This amends [image.md](../image.md)'s statement that credentials are read once at start; no earlier ADR states it.
2. **`registry.credential_helpers` maps a registry host to a helper name.** A pull from that host runs `docker-credential-<name> get`, found on mvmd's `PATH`, at each lookup, with the server URL on standard input and `{"ServerURL","Username","Secret"}` on standard output, bounded by the pull's timeout. The helper runs with mvmd's environment as it is, unfiltered, and may run other programs. A host named in both the credentials file and the helpers is refused, at start and at any read that finds it. Neither the helper's standard output nor its standard error is logged or placed in an error.
3. **`registry.ca_file` is the absolute path of a PEM bundle** added to the system roots for registry TLS. It is read at each request, so a rewrite takes effect without a restart, and refused at start when it is not a regular file or holds no certificate.

## Consequences

- A rewritten credentials file, a helper's new answer or a rewritten CA bundle reaches the next request with no restart and no watch.
- Each lookup reads a file or starts a process. A bearer token or basic header the client holds is used until the registry refuses it, so a lookup happens at a refusal, and a pull makes few of them.
- The helper's environment is mvmd's own, so `AWS_PROFILE`, `AWS_CONFIG_FILE` and the like reach it, and so does every other variable mvmd holds.
- A helper that fails fails the pull, and the error names the helper and the registry, not what it wrote.

## Signs the decision was wrong

- A helper's cost per lookup (an STS call) makes pulls slow enough that the cache a helper keeps is not enough, so mvmd should cache what it returns for a lifetime the helper states.
- A secret reaches a log or an error through a path this decision did not name, such as a helper's descendant writing to mvmd's own file descriptors.
- A node needs a helper's environment narrower than mvmd's, so the unfiltered inheritance was the wrong default.
