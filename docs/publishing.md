# Publishing

What `.github/workflows/publish.yml` pushes, where, and how a consuming
repository reads it without a credential of its own.

## Why an image, and not a release asset

Release assets on a private repository need a credential to fetch; GHCR is
the one place on GitHub where another repository's workflow can read with
nothing but its own `GITHUB_TOKEN` — a package grants a repository Actions
access with the read role, and that grant is enough. steading#6's browser
suite starts a real `dg serve` and `mill serve` in its own CI and needs both
binaries this way, so mill publishes what it builds as an image and
steading pulls it.

## Image and tags

`ghcr.io/steading-ai/mill`, built for `linux/amd64` and `linux/arm64` on
every push to `main` — never on a pull request; the workflow is a publish,
not a check. Two tags move on each push:

- `:<sha>` — the full commit SHA, one tag per push, never reused.
- `:main` — moved to whatever `:<sha>` the latest push produced.

## What the image carries

It is a distribution of files, not a service definition
(`packaging/Containerfile`, built from `scratch`):

| Path | What it is |
|---|---|
| `/usr/local/bin/mill` | the binary, stamped with the same version a local build of that commit would carry (`make version`) |
| `/usr/local/libexec/mill/sidecar` | the built harness sidecar and its production dependencies |
| `/usr/local/share/mill/workflows` | the workflow definitions and skills |

`org.opencontainers.image.revision` carries the commit SHA it was built
from. A consumer copies the files out — `podman create` + `podman cp`, or
`oras` — or runs the image directly and invokes `mill` from its `PATH`, as
it chooses; nothing in the image itself starts a service.

## The one-time access grant

Publishing the image is not the same as a consuming repository being able
to read it: a person grants that once, by hand, in the package's own GitHub
settings — this is a settings page, not an API, so it cannot be part of the
workflow. On `ghcr.io/steading-ai/mill`'s package page: **Package settings
→ Manage Actions access → Add repository**, naming the consuming repository
(steading) with the **Read** role. Once granted, that repository's own
`GITHUB_TOKEN` can `docker login ghcr.io` and pull the image in its own
workflow, with no secret shared between the two repositories.
