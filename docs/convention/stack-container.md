# Container Images

What an image is built from and what it may carry. Applies to every
OCI image the project builds, whatever builds it.

> Checked against the Dockerfile frontend
> `docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e`,
> run by BuildKit v0.33.1. A claim below that names no version holds
> for these.

- **`FROM <image>@sha256:…`.** A tag is a name that can be moved to
  different content; a digest is the content. Two builds a month apart
  from the same tag are not the same base, and nothing in the file
  records that they differ. Take the digest of the multi-platform
  index, not of one platform's manifest: the mismatch is a warning and
  an exit code of zero, and the image that ships is labelled for a
  platform it was not built on.
- **A build secret arrives on a mount, never in a layer.**
  `RUN --mount=type=secret,id=…` exists so a secret is not baked into
  the image or the build cache. Anything a stage copies in is a layer,
  and a layer is readable by whoever can pull the image — a later
  instruction that removes the file does not remove the layer that
  holds it.
- **Two images stay two when one holds what the other may not reach.**
  Merging gives the merged image both. The case for merging is about
  the build — one fewer thing to build and pull — and the reason they
  were separate is about what runs, so the two are not answered in
  the same terms. Write that reason where the merge will be proposed:
  the responsibility document for what the image runs, not a comment
  inside the Dockerfile.
