# The base image

What every machine boots from: Fedora with systemd as PID 1, rootless
podman for the session user, git, sudo, mise with node for the harness, and
the toolchain native extensions compile against. Specified in
`docs/sandbox.md` §Images; rehearsed by the nspawn spike
(`docs/spikes/nspawn-per-run-sandbox.md`, E2, E3, E10, E12).

The operator owns it. It carries nothing of a run's: no credential, no
skill, no sidecar, none of the operator's `$HOME`. Those are the worker's
and are mounted at provision.

## Build and publish

```sh
mill image build-base            # localhost build, tagged <registry>/mill/base:<date> and :latest
mill image build-base --push     # and pushed with $CREDENTIALS_DIRECTORY/harbor-robot
```

`contrib/systemd/mill-image-build.timer` runs the second weekly on the host,
as the operator's user unit, from this repository's checkout. Not forge CI:
the host that runs machines is the host with podman and the credential.

The Harbor host is the `HARBOR_HOST` build argument. It names the Fedora
proxy project the build pulls from (`fedora-cache`; the host's `policy.json`
rejects `registry.fedoraproject.org` directly) and the three proxy projects
`registries.conf` routes `docker.io`, `quay.io` and `ghcr.io` through inside
the machine — `dockerhub-cache`, `quay-cache`, `ghcr-cache`, the same names
the host's own `registries.conf` uses.

## What is in it

| Path | What |
|---|---|
| `agent`, uid 1000, home `/home/agent` | the session user; `/work` is its checkout mount, `mise trust` pre-granted |
| `/etc/subuid`, `/etc/subgid` | `agent:10000:55536` — inside the machine's own 65536 uids, so rootless podman can write its map |
| `/usr/local/bin/dnf` | the shim: `sudo /usr/bin/dnf install`, a `report.install` to `/run/mill/ws.sock` with the session's `STEP_TOKEN` when both are there, the `[image].packages` hint as the last three lines |
| `/etc/sudoers.d/agent-dnf` | `agent ALL=(root) NOPASSWD: /usr/bin/dnf install *` — the only sudo path |
| `/etc/containers/storage.conf` | `overlay` under the ephemeral root |
| `/etc/containers/containers.conf` | `cgroups = "split"`: containers under the step's slice |
| `/etc/containers/registries.conf.d/harbor.conf` | the proxy projects |
| `/etc/systemd/system/mill-proc.service` | a pristine `proc` over the one nspawn masked, before `agent`'s user manager |
| `podman.socket` | enabled at the system level and in `agent`'s user manager, which lingers |
| `/usr/local/bin/mise`, `/usr/local/bin/node` | mise, and node installed through it |
| `/etc/machine-id` | empty, so each boot generates one |

## What export must do

`podman export` writes a flat tar and drops file capabilities. The worker's
export step — `internal/workspace/machine/image`, after the tar is
extracted into the subvolume — restores the two rootless podman needs:

```sh
setcap cap_setuid=ep <tree>/usr/bin/newuidmap
setcap cap_setgid=ep <tree>/usr/bin/newgidmap
```

That is the export's job and not this Containerfile's, because the
Containerfile's result is an OCI image and the capabilities do not survive
the trip out of it. Everything else the spike found the tree needs — the
subordinate ranges, the `proc` unit, `cgroups = "split"` — is baked here.

## Checking a build by hand

```sh
podman build --build-arg HARBOR_HOST=harbor.<tailnet> -t mill-base-check images/base
podman run --rm mill-base-check sh -c 'id agent; cat /etc/sudoers.d/agent-dnf; systemctl is-enabled podman.socket'
```

The live spec in `internal/workspace/machine/image` does the same and more
under `MILL_SANDBOX_HOST=1`: it builds the image, exports it through the
worker's own pipeline and reads `getcap` off the exported tree.
