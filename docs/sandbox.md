# Per-run sandbox

How a run's workspace becomes a `systemd-nspawn` machine: the image it boots, the caches it restores, what mill does to it, and what it may assume about it. Decided in [ADR-018](adrs/018-a-runs-sandbox-is-a-systemd-nspawn-machine.md); specified as behaviour in [the scenarios](sandbox-scenarios.md); validated by a spike run on `core` by a person (§The host). RFC 2119 keywords are normative.

This spec describes the backend behind `Workspace`. It does not describe the `Workspace` interface's Go shape, which [Architecture §The workspace](architecture.md#the-workspace) and `internal/workspace` own; where this document names an operation it names a capability the interface must expose, not a method signature.

## Scope

In scope: the run's sequence from claim to destroy; base and derived images; `mill.toml`; the machine's lifecycle, exec channel and step boundaries; filesystem, caches and eviction; network; credentials; session identity; the `dnf` shim; preparatory steps; resource limits; privilege; reconciliation on resume; failure modes.

Out of scope: the harness sidecar and its wire protocol; what a workflow's steps are; the multi-tenant boundary (`systemd-vmspawn`), which this design admits later without changing the interface.

## The host

The worker runs on `core`, the operator's host, and the design assumes its facts. They are stated here once and referred to by name everywhere else.

- Fedora 44; systemd 259 with `systemd-nspawn` and `systemd-vmspawn`; `systemd-container` installed. SELinux enforcing. Bare metal with `/dev/kvm` (mode 0666).
- Rootless podman 5.8. A Harbor registry with proxy-cache projects for `docker.io`, `quay.io` and `ghcr.io`.
- The root filesystem is XFS on LVM on LUKS with no free extents. A second NVMe, `nvme1n1` (931 GB), is the sandbox's own volume (§Storage).
- firewalld owns `inet firewalld`; NetworkManager manages links unless told otherwise. Every polkit action the design names is present: `org.freedesktop.machine1.{shell,manage-machines,login,open-pty}` and `org.freedesktop.systemd1.manage-units`.
- Host configuration mill cannot ship — the storage volume, the machine template unit and its polkit rule, the nftables table, squid, the SELinux file context, the credential delivered by `LoadCredential` — is homestead's, the operator's host-configuration project, and reaches `core` as roles there (§"What the host provides").

**The spike.** The experiments this design depends on — numbered E0 to E14 below — were run on `core` by a person and are written up in `docs/spikes/nspawn-per-run-sandbox.md`; a section that names one states what the experiment found, and the sections the backend is built on — §Provision, §Exec, §Step boundary, §Filesystem, §Destroy, §Privilege — keep no sentence the spike disproved.

## Terms

| Term | Meaning |
|---|---|
| **base image** | The operator's image: Fedora, systemd, podman, mise, the native-extension toolchain. Built from a Containerfile in the mill repo, published to Harbor, resolved to a digest per run. |
| **derived image** | Base plus the system packages a repo's `mill.toml` declares. Identified by `hash(base digest, sorted packages)`. A repo declaring no packages has a derived digest equal to the base digest. |
| **image subvolume** | A derived image exported to a btrfs subvolume under `/var/lib/machines/`. What a machine boots from. |
| **machine** | One `systemd-nspawn` instance, booted (`-b`) from an ephemeral snapshot of an image subvolume, registered with `systemd-machined` under the run's name. One per run. |
| **machine unit** | The host unit that owns the machine's process tree and resource limits: one instance of the `mill-machine@.service` template, started and stopped by the worker through polkit. Stopping it destroys the machine. |
| **step slice** | A slice inside the machine's systemd under which every process of one step runs. Stopping it reaps the step. |
| **exec** | A transient service inside the machine, addressed by machine name, with piped stdio and an exit code. `Workspace.Exec` realised. |
| **workspace dir** | A host directory per run, bind-mounted into the machine at `/work`. The checkout lives here. |
| **workspace socket** | A unix socket the host serves, bind-mounted into the machine. The channel a session uses to reach mill and request credentials. |
| **lineage** | A named cache: a sequence of read-only btrfs snapshots (generations) of one directory, plus a manifest. Scoped to a repo or to the worker. |
| **generation** | One read-only snapshot in a lineage, produced by promotion. |
| **promotion** | Snapshotting a run's writable copy of a cache as the lineage's next generation. Happens only from a step that closed on evidence. |
| **preparatory step** | A deterministic step that readies the machine — image build, toolchain install — whose failure becomes a finding for the first session rather than a failed run. |
| **mill.toml** | The repo's declaration: system packages and cache paths. Read from the checkout before the machine boots. |

## Naming

- Machine: `mill-<id>`, where `<id>` is the run id in lower case (§Provision). The reconciliation key; **MUST** be derivable from the run alone.
- Machine unit: `mill-machine@<run-id>.service` on the host, an instance of the root-owned template (§Privilege).
- Step slice: `mill-step-<ordinal>.slice` inside the machine.
- Exec unit: `mill-exec-<uuid>.service` inside the machine, under its step's slice.
- Image subvolume: `/var/lib/machines/mill-image-<derived-digest>`.
- Lineage: `/var/lib/mill/cache/<scope>/<name>/` where scope is `repo/<repo-id>` or `global`; generations are `gen-<n>` inside it; `manifest.json` beside them.
- Run directory: `/var/lib/mill/runs/<id>/` containing `workspace/`, `cache/<name>/` (the run's writable snapshots), `machine.json`, `run.json` and `environment.d/`.

## Sequence of a run

Everything below is in this order. The order is the design: configuration is read before the thing it configures exists, and nothing persists except through promotion.

1. **Claim.** The run exists in the ledger; the run directory is created.
2. **Clone.** The worker clones the node's repository into the workspace dir on the host, with the worker's own forge credential, bound to no step. No session credential is involved; `Rotate` has not been called.
3. **Read `mill.toml`** from the checkout. Absent is valid and means defaults. Invalid is a finding (§mill.toml), not a failure.
4. **Resolve the base** digest from Harbor (`mill/base:latest` unless worker config pins a tag or digest). Compute the derived digest.
5. **Ensure the image subvolume** (§Derived images). On build failure, fall back to base and record a finding.
6. **Restore caches** (§Caching): a writable snapshot per lineage into the run directory.
7. **Boot the machine** (§Provision): `machine.json` with the workspace, socket, skill, sidecar, workflow, cache and proxy binds, then `systemctl start mill-machine@<id>`.
8. **Wait for ready** — `is-system-running` inside — then start the socket and record the veth.
9. **Toolchain step** (§Preparatory steps): `mise trust /work && mise install`.
10. **Workflow steps**, each under its own slice; caches snapshotted at each evidence-closed step.
11. **Finish**: promote the caches from the last evidence-closed step; record the result; destroy the machine; remove the run directory.

## Images

### Base

- Built from `images/base/Containerfile` by `mill image build-base --push`, run weekly by `contrib/systemd/mill-image-build.timer` (a user unit beside `contrib/systemd/mill.service`; on `core` homestead's `mill` role renders it as `mill-image.timer`) and on demand; pushed to Harbor as `mill/base:<date>` and `mill/base:latest` with a robot credential delivered by `LoadCredential` as `$CREDENTIALS_DIRECTORY/harbor-robot`. `images/base/README.md` lists what is in it and what the export step must do to the tree. **Not by forge CI**: CI is not active on the GitLab projects, and the host that runs machines is the host that has podman and the credential.
- **MUST** contain: systemd as PID 1, `dbus-broker`, `systemd-networkd` enabled (so `host0` takes the host's DHCP lease through systemd's own `80-container-host0.network`; Fedora's preset leaves it off and a machine without it has no route out), `podman` with `podman.socket` enabled and `storage.conf` selecting `overlay`, `git`, `sudo`, `mise`, and the native-extension toolchain — gcc, g++, make, pkgconf, python3, and the `-devel` set ruby-build, node-gyp and linked crates require (openssl, zlib, libffi, libyaml, readline, ncurses, gdbm, sqlite). Node installed globally through mise for the harness's runtime.
- **MUST** contain the user `agent` (uid 1000, home `/home/agent`), the `dnf` shim (§The dnf shim) and its sudoers rule, `registries.conf` routing pulls through Harbor's proxy projects, and a pre-granted `mise trust` for `/work`.
- **MUST NOT** contain credentials, the mill skill, the sidecar, or any of the operator's `$HOME`. The skill and sidecar are the worker's and are mounted at provision.
- The worker resolves the base digest at each provision. A new base takes effect on the next run; rollback is a pin in worker config (`base-image`, a tag or a digest).
- What the spike found the exported tree needs, and where each lands: the file capabilities on `newuidmap` and `newgidmap` are restored by the export step, because `podman export` drops them and the Containerfile cannot put them back; `agent`'s subordinate ranges (`10000:55536`, inside the machine's 65536 uids), the `mill-proc.service` unit that mounts a pristine `proc` before `agent`'s user manager, and `containers.conf` with `cgroups = "split"` are baked into the image.

### Derived images

- The derived digest is `sha256(base digest ‖ sorted, deduplicated package list)`. The `nested` variant (§Self-hosting) hashes its own name after the list — `sha256(base ‖ packages ‖ "+nested")` — and its list carries `btrfs-progs` and `systemd-container` whether or not the repo declared them, so a nested image and the flat image of the same packages are two subvolumes; a nested request with no packages is still not the base.
- The build is on demand (`internal/workspace/machine/image`, `Ensure`):
  1. Take `flock` on `/var/lib/mill/images/<digest>.lock`. A concurrent run of the same repo waits here. The lock is held on a descriptor podman never inherits: `flock <file> cmd` leaks it into `pasta`, which logs a denial per build.
  2. Under the lock, if `mill-image-<digest>` exists, use it.
  3. Otherwise generate `FROM <harbor>/mill/base@<digest>` + `RUN dnf install -y <packages>` — plus, for the `nested` variant, the sudoers line `agent ALL=(ALL) NOPASSWD: ALL` in `/etc/sudoers.d/agent-nested`, the only image that carries it — `podman build` it on the host (through the workspace chokepoint; this is the one place the worker runs a container outside a machine, and the code **MUST** make that visible), export the container's filesystem into `mill-image-<digest>.building` as a subvolume — `podman create`, `podman export | tar -x`, a flat tree and not layers — restore the two file capabilities, then rename atomically to `mill-image-<digest>`. The export is one function so that a label for a confined machine (§SELinux) has one place to go.
  4. Release the lock.
- The image directory is root's (§Storage), so the subvolume steps of the export run through `sudo`; the worker's own record of each image — base digest, package list, built-at, last-booted-at — is `/var/lib/mill/images/<digest>.json`, beside the lock, and `mill image ls` reads those.
- The build has a time budget from worker config. A build that fails or exceeds it **MUST** release the lock, remove `.building`, boot the run from the **base** subvolume, and attach a finding to the first session: the `dnf` error's last lines and the package list that failed. This covers a misspelled package and a distro that dropped one alike.
- Derived images are not pushed to Harbor. A second worker rebuilds them; base is the artefact worth distributing.
- Image subvolumes are a lineage for eviction purposes (§Eviction): last-used at each boot, 30-day expiry, evicted under the same watermarks. A base bump makes the previous derived images unused; they expire on their own.
- `*.building` leftovers are removed at worker start (§Reconciliation).

## mill.toml

At the repo root, beside `mise.toml`. Read at step 3 of the sequence, from the checkout. Absent means defaults.

```toml
[image]
packages = ["libpq-devel", "vips-devel"]

[cache.go-build]
path = "~/.cache/go-build"

[cache.target]
path = "/work/target"
key  = ["rust-toolchain.toml"]   # optional; see §Caching

[cache.go-mod]
enabled = false                  # opt out of a default

[machine]
nested = true                    # only for a repo whose tests boot machines; see §Self-hosting
```

- `[image].packages` is a list of distro package names. Nothing else about the image is configurable from the repo.
- `[machine].nested` requests that this repo's machine may boot machines of its own. It is a request; worker config grants it per repo, and an ungranted request is a finding. What it changes is in §Provision and §Self-hosting.
- `[cache.<name>].path` **MUST** resolve under `/home/agent` (`~`) or `/work`. Any other path is a validation error. Paths **MUST NOT** be `/work` itself, `/home/agent` itself, or `/run/mill`.
- `[cache.<name>].key` is an optional list of files in the checkout. When present, the lineage restores only if the hash of those files matches the hash recorded at the latest generation's promotion; otherwise the run starts that cache empty. It exists for a tool whose cache is not self-invalidating; the defaults do not set it.
- `[cache.<name>].enabled = false` disables a default.
- Default caches come from the ecosystems `mise.toml` declares (§Caching). A `[cache.<name>]` with a default's name overrides it.
- Validation errors **MUST NOT** fail the run. The run proceeds with defaults and the first session receives the error as a finding. The agent that wrote the config is the one that fixes it.
- Authority: the repo declares *what* is cached and *which* packages; the worker owns policy — retention, size, watermarks, whether promotion is allowed, time budgets. Nothing in `mill.toml` can mount a host path, exceed a limit, or persist state except through promotion.
- The mill skill (§Skill mount) documents this file; it is where an agent learns to write it.

### Grammar

What the rules above leave open, `internal/workspace/machine/config` decides as follows.

- The file is read from `mill.toml` at the checkout root and nothing else; the ecosystem defaults come from `mise.toml` beside it, its `[tools]` table only, whatever shape each tool's value takes. A tool named through mise's `core:` backend counts as its bare name. No other mise config location is consulted, and no path is resolved through `mise env` or `go env`.
- The tables are `[image]`, `[cache.<name>]` and `[machine]`. Any other key, at any depth, is a finding naming the outermost unknown key and is ignored.
- `[image].packages` entries **MUST** match `^[A-Za-z0-9._+-]+$`; the list is sorted and deduplicated, and an entry that does not match is a finding and is dropped.
- `<name>` in `[cache.<name>]` **MUST** match `^[A-Za-z0-9][A-Za-z0-9._-]*$`: it becomes a path component on the host. `~` in `path` expands to `/home/agent`; the path is cleaned before the rules apply, so `/work/../etc` is `/etc`. A relative path is a finding.
- Scope is not declarable. A cache with a default's name keeps the default's scope (and its path, when the declaration omits `path`); a new name is `repo`-scoped, because the worker, not the repo, decides what is shared across repositories.
- `key` entries are paths relative to the checkout root; an absolute path or one that escapes the checkout is a finding.
- A `[cache.<name>]` with a finding is dropped whole; the rest of the file still applies. A file that does not parse is one finding carrying the parse error, and the run's configuration is the defaults.
- A finding names the file, the key in dotted form (`cache.go-build.path`), and one sentence to act on.

## Lifecycle

### Provision

The worker writes `/var/lib/mill/runs/<id>/machine.json` and starts `mill-machine@<id>.service` with `systemctl start`, under polkit (§Privilege). It never runs `systemd-nspawn` itself: the template's `ExecStart` is homestead's helper, `/usr/libexec/mill-machine <id>`, which reads that file, refuses anything outside the schema below with the field named in `journalctl -u mill-machine@<id>`, applies the limits to the unit, and execs

```
systemd-nspawn --boot --quiet --keep-unit \
    --machine=mill-<id> \
    --directory=/var/lib/machines/mill-image-<derived-digest> --ephemeral \
    --private-users=pick --private-users-ownership=auto \
    --network-veth --settings=false --notify-ready=yes \
    --bind=<source>:<target>:idmap ...        # one per bind; --bind-ro when readonly
    --bind-ro=/sys:/.hostsys                  # nested only; no :idmap, sysfs cannot be idmapped
```

- `<id>` is the run id in lower case: the helper's instance pattern is `^[a-z0-9-]+$` and a ULID's alphabet is upper case; base32 is case-insensitive, so nothing is lost. `exec`, `stop`, `terminate` and `shutdown` are the helper's verbs and cannot be ids. The machine is `mill-<id>`, the unit `mill-machine@<id>.service`, the run directory `runs/<id>`, the socket directory `/run/mill/<id>`.
- `--ephemeral` **MUST** be used: the root is a btrfs snapshot of the image subvolume, `.#machine.mill-image-<digest>…` in `btrfs subvolume list`, discarded on exit.
- `--keep-unit` keeps the machine's whole process tree in the unit's cgroup, `mill-machine@<id>.service/payload/`. Without it `--boot` registers the payload in a separate `mill-<id>.scope` that neither `KillMode=` nor the limits reach — the root cause of the spike's E0 orphan. Reconciliation and limits read the payload there, not in a scope.
- `--notify-ready=yes` makes the unit's `Type=notify` mean the machine's systemd is up, so `systemctl start` returns once the machine can be entered, not once nspawn has forked.
- `--private-users=pick` allocates a 64K uid range per machine; two machines never share one. It is the template's business, not the worker's.
- `--private-users-ownership=auto` idmaps the root tree **only**. Every bind carries `:idmap` (the spike's E4), or a file written as uid 1000 inside is an offset uid on the host; the helper adds it to every bind it admits.
- `--settings=false` stops `.nspawn` files on the host from altering the invocation; everything the machine is, is in `machine.json` and therefore in the ledger.
- **Every bind a run needs is in `machine.json` before the unit starts.** `machinectl bind` refuses a user-namespaced machine, so nothing can be added later.
- When `[machine].nested` is granted — worker configuration lists the repository under `nested_repos` (§Self-hosting) — `nested` is true in the manifest and three things change: the machine boots the `nested` image variant (§Derived images), in which `agent` has passwordless `sudo` for everything — root inside the user namespace is an unprivileged uid on the host and the namespace, not the uid, is the boundary — and which carries `systemd-container` and `btrfs-progs`; the host's image subvolumes are bound read-only at `/var/lib/machines` so the inner suite boots them rather than building them, and the dagger checkout (`dagger_checkout`) read-only at `/var/lib/mill/dagger` for its test fixture; and the helper adds `--bind-ro=/sys:/.hostsys` — the host's `sysfs`, bound without `:idmap`, which `sysfs` refuses (`Failed to clone /sys: Invalid argument`) — because an inner nspawn cannot mount `proc` or `sysfs` unless a fully visible one is already in its tree when it pivots, and the outer's own `/sys` is nspawn's tmpfs of subtrees, not one (the spike's E13; §Self-hosting has the inner's side). The uid mapping does not change: a nested machine boots `--private-users=pick` like every other, its 65536 uids enough, because the inner must run `--private-users=identity` — `pick` picks outside the outer's map and idmapped mounts do not stack, `chown` dies on nspawn's base offset — and identity needs no room of its own; a wider range for the outer would buy nothing. A request that is not granted is a finding for the first session and the machine boots without it. The host's btrfs **MUST** be mounted with `user_subvol_rm_allowed`, or inner teardown and eviction cannot delete subvolumes.

#### `machine.json`

The schema is the helper's (homestead's `mill_machine` role, `/etc/mill-machine.json` its policy); `Manifest` in `internal/workspace/machine` is the Go type and carries exactly these keys. Every key but `image` is optional; an unknown key at any level is refused.

```json
{
  "image": "3c9a…",
  "binds": [
    {"source": "/var/lib/mill/runs/<id>/workspace", "target": "/work"},
    {"source": "/run/mill/<id>", "target": "/run/mill"},
    {"source": "/home/ivy/.local/share/mill/skill", "target": "/home/agent/.claude/skills/mill", "readonly": true},
    {"source": "/home/ivy/.local/share/mill/sidecar", "target": "/home/agent/.local/share/mill/sidecar", "readonly": true},
    {"source": "/home/ivy/.local/share/mill/workflow", "target": "/home/agent/.local/share/mill/workflow", "readonly": true},
    {"source": "/var/lib/mill/runs/<id>/cache/go-build", "target": "/home/agent/.cache/go-build"},
    {"source": "/var/lib/mill/runs/<id>/environment.d/50-mill-proxy.conf", "target": "/etc/environment.d/50-mill-proxy.conf", "readonly": true}
  ],
  "limits": {"MemoryMax": "8G", "CPUQuota": "400%", "TasksMax": "4096"},
  "nested": false
}
```

| Key | Rule |
|---|---|
| `image` | `^[a-z0-9]+$`; `/var/lib/machines/mill-image-<image>` must exist. The worker writes the digest `image.Ensure` returned — the derived one, or base after a failed build. |
| `binds[].source` | An absolute, normalised host path that exists. After symlinks are resolved it must be under `/var/lib/mill/runs/<id>/` or `/run/mill/<id>/`, or under one of the policy's read-only sources (the worker's share root, `~/.local/share/mill`, where the skill, the sidecar and the workflow copy live), or — only when `nested` is true — one of its nested read-only sources (`/var/lib/machines`, the dagger checkout). The latter two groups must be declared `readonly`. A source under `/sys` is refused by name, nested or not: the host's `sysfs` enters a machine only as the helper's own `/.hostsys` bind. |
| `binds[].target` | An absolute, normalised path inside the machine, not `/`; `/.hostsys` is reserved for the helper's bind. |
| `binds[].readonly` | Boolean, default `false`; `--bind-ro`. |
| `limits.MemoryMax`, `limits.CPUQuota`, `limits.TasksMax` | Strings in the forms `systemd.resource-control(5)` accepts. A limit that is absent is reset to the unit's default on every start. |
| `nested` | Boolean, default `false`. |

The worker's binds are: the workspace at `/work`; the socket directory at `/run/mill`; the skill read-only at `/home/agent/.claude/skills/mill`; the sidecar read-only at `/home/agent/.local/share/mill/sidecar`; the workflow directory read-only at `/home/agent/.local/share/mill/workflow`, when the worker loaded one (§"Skill mount"); one per cache, `runs/<id>/cache/<name>` at the lineage's path; the proxy file read-only at `/etc/environment.d/50-mill-proxy.conf` (§Network); and, under `nested` and only then, `/var/lib/machines` read-only at `/var/lib/machines` and the dagger checkout read-only at `/var/lib/mill/dagger`. Those two are the whole difference between a nested and a flat `machine.json` beside `nested` itself and the image digest (the variant), and the validator admits them only with `nested` true: homestead's `mill-machine` owns that check, this schema documents it. `Manifest.Check` applies the same rules before the unit is started, so a bind the helper would refuse fails in the worker with the field named rather than in the unit's journal — a `dagger_checkout` that is not a directory is refused there too. The validator is the authority: a field the worker needs that the schema lacks is a homestead change, not a divergent writer.

#### Readiness

`systemctl start` returning is the first signal; the worker then polls `machinectl show mill-<id> -p State` for `running` and execs `systemctl is-system-running --wait` inside the machine through the helper, under the slice `mill-step-ready.slice` — a step slice like any other, so the rule that no exec runs outside one holds for the one exec that precedes every step. `running` is booted; so is `degraded`, a machine that booted with a failed unit. Booted is not yet ready: systemd's state lands before the DHCP lease on `host0` does, so the worker then execs `systemd-networkd-wait-online --interface=host0 --ipv4 --operational-state=routable` under the same slice, bounded to a few seconds per probe, and the machine is ready when it exits zero: routable is the DHCP lease, which lands about two seconds after the link-local address that satisfies `--ipv4` alone, and before it the first step's `mise install` runs with no route to the proxy. A helper refusal (exit 78) ends the wait at once: no number of retries changes the helper's answer. Readiness has a timeout (`ReadyTimeout`, two minutes unless configured); a machine that does not reach it is destroyed by the unready path of §Destroy and the run fails with the timeout and the digest as evidence. On `core` a machine is ready 0.4 s after `Create` begins.

Once ready, the worker records the host-side veth (`machinectl status` `Iface:`), records the boot against the image's manifest, and starts the workspace socket on `/run/mill/<id>/ws.sock` — short and fixed on the machine side at `/run/mill/ws.sock`; only the host side needs the existing length guard. `Attach` reads `runs/<id>/run.json`, asks machined for the machine, and reopens the socket; a machine machined no longer knows is a lost sandbox, and nothing boots a fresh one for a run that had one (§Reconciliation).

### Exec

Every process a step runs, including checks mill runs itself, enters the machine through the root-owned helper under `sudo -n` — never through `systemd-run -M` as the operator, which opens the leader's namespaces itself and wants `CAP_SYS_ADMIN` whatever polkit says (the spike's E0):

```
sudo -n /usr/libexec/mill-machine exec mill-<id> --unit=mill-exec-<uuid> \
    -p Slice=mill-step-<n>.slice -p WorkingDirectory=/work -p User=agent \
    [-p ReadOnlyPaths=/work] -p Environment=<k=v> ... -- <argv>
```

The helper runs `systemd-run -M mill-<id> --pipe --wait --quiet --collect -p Delegate=yes -p KillMode=mixed` with those properties and streams stdio through.

- `--pipe` connects the caller's stdin, stdout and stderr to the service. Mill **MUST** stream them.
- `--wait` propagates the main process's exit status. That status is what `Workspace.Exec` returns and what evidence checks read. The helper's own refusals exit 78 before anything runs and are never the command's status.
- `--quiet` keeps `systemd-run`'s own messages off the step's stderr. `--collect` unloads the unit when it ends.
- `Delegate=yes` on every exec unit, so `agent`'s podman can create sub-cgroups under it; `KillMode=mixed`, so what the exec leaves behind is SIGKILLed at once rather than after `TimeoutStopSec`, and the status `--wait` returns stays the main process's (the spike's E3).
- `Slice=` is **REQUIRED** and **MUST** match `^mill-step-[a-z0-9-]+\.slice$`. An exec with no step slice is a bug. `Slice`, `WorkingDirectory`, `User`, `Environment` and `ReadOnlyPaths` are the whole allow-list: a property that could load a unit file or run something on the host is refused by name.
- **A token without `write` runs against a read-only checkout.** When the step token the workspace holds does not grant `write` — a reviewer, a check, the toolchain step, or no token at all — every exec under it carries `ReadOnlyPaths=/work`, and the helper admits exactly that value. systemd gives the unit and everything it spawns a mount namespace of its own in which `/work` is read-only; `$HOME`, `/tmp` and the cache binds stay writable, so a build or a test suite runs, and the machine's own `/work` and the host's copy are untouched. Verified on `core`: under the property `touch /work/.probe` fails with `Read-only file system`, `touch /tmp/.probe` and a write into `~/.cache/go-build` succeed, and no `.probe` appears on the host. The capability set is the contract, whatever the step kind: the socket enforces it for the verbs, and this enforces it for the SDK's own file tools and the shell, which never reach the socket (`docs/domain-model.md` §"Capability set"). It is why a reviewer may have a shell.
- Cancellation is the helper's `stop mill-<id> mill-exec-<uuid>.service`, or the slice. Killing the client does not stop the unit.
- The exec's `Environment=` is its only environment. There is no machine-wide environment for steps; the one exception is `agent`'s user manager, which carries the proxy from boot (§Network).
- **The session's sidecar is such an exec.** A skill step runs as a headless coding-agent session, and that session's sidecar (`internal/harness/claude`, §"Skill steps: the harness adapter" in `docs/architecture.md`) is spawned through the same `Workspace.Exec` a check goes through: under the step's slice, as `agent`, in `/work`, with `Environment=` its whole environment. It is not spawned on the host and it is not special; it is reaped when the step's slice stops, like everything else the step ran. The mounted sidecar it runs and its entrypoint are §"Skill mount".
- On `core` today `/etc/sudoers.d/ivy` grants the operator `NOPASSWD: ALL`, so for that uid the helper is the *interface* the worker uses, not a boundary sudo enforces; `/etc/sudoers.d/mill-machine` is the boundary for any other uid, and it admits `exec`, `stop` and `terminate` and nothing else.

### Verbs

The seven verbs a session reaches over the socket — `git.status`, `git.diff`, `git.log`, `git.commit`, `git.push`, `forge.pr`, `forge.checks` — run **on the host**, against `runs/<id>/workspace`, and never through the helper. They are the same code the local backend runs against its checkout (`internal/workspace/verbs`, which both backends import): the same signing key, the same forge client, the same identity.

- **Why the host.** The bind is idmapped (§Filesystem): a file written as `agent` inside is owned by the worker's uid outside, so the worker can stage and commit what a session wrote without a second identity or a second checkout. The session never sees a credential it could keep (docs/core-principles.md §9), and nothing about git needs to enter the machine to do its job, so nothing does: the signing key, the forge token and the mill's git configuration stay on the host. The machine is where the session works; the host is where the mill signs.
- **Where each verb runs, and why.** `status`, `diff` and `log` are read-only and could run in either place; they run on the host too, for one code path. `commit` stages the paths the session named — or everything — and commits on the run's branch with the mill's identity and the mill's key, `-c user.signingkey=` and `-c gpg.ssh.allowedSignersFile=` per invocation, so a commit made through the verb verifies on the host with `git -C runs/<id>/workspace verify-commit HEAD`. `push`, `pr` and `checks` present the worker's forge credential, minted for the step whose token is live, through the environment and a credential helper: never argv, never a file under `/work`.
- **The key.** The mill account's one signing key, the one every project's `allowed_signers` names and `mill doctor` probes: `signing/` under `--workspace-dir`, where the local backend keeps it (worker configuration's `SigningDir`; `<sandbox root>/signing` if nothing names one). It is generated there on the first run that needs it and is never among a machine's binds.
- **A session's own `git commit` is unsigned.** The exec environment carries `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` with the mill's identity, so a commit a session makes on its own is attributed correctly — but no key reaches inside, and no host path does, so it is not signed and nothing re-signs it. **The push verb refuses it**: every commit the push would publish — reachable from `HEAD` and from no remote-tracking ref — must carry a good signature under the mill's `allowed_signers`, and the first that does not fails the push with `workspace: refusing to push a commit the mill did not sign`, naming the commit and the verb to use. A session that committed on its own resets to the base and commits through `git.commit`; the skill says to use the verbs in the first place. Re-signing at push was the alternative and was rejected: it rewrites every SHA the session, the `commit` evidence and the pull request already hold.
- A session's own `git push` inside the machine has no credential and fails. That is the design, not a bug.

### Step boundary

- Each step runs under a slice of its own, `mill-step-<ordinal>.slice`, where the ordinal counts the steps a worker has begun — a retry is a fresh ordinal, because the previous attempt's slice was reaped. The run loop calls `BeginStep(n)` before a step and `EndStep(n, closedOnEvidence)` after it, whichever way the step went; a backend with no slice — the local one — is asked neither. The workspace holds the current slice, and an exec with none is refused (§Exec).
- A step **MUST** end with the helper's `stop mill-<id> mill-step-<n>.slice`: `systemctl -M stop` of the slice, then `1` written to the slice's `cgroup.kill` on the host, because `systemctl stop <slice>` does not kill orphans of already-ended units and `cgroup.kill` does (the spike's E3). This kills everything in the slice: every process the step started, its daemons, and a container it started with `podman run` from its own cgroup. The helper's stop is synchronous, so the slice is inactive once `EndStep` returns.
- **What the slice does not hold.** A container started through the machine's podman API socket — what testcontainers does — is created by `podman.service` under `agent`'s user manager, not under the caller's cgroup, so it lands under `user@1000.service` and the step boundary does not reap it; machine teardown does, with the ephemeral root. For the db suite that gap is closed by testcontainers' own reaper, ryuk, which removes what a suite started once the suite's connection drops (its `RYUK_RECONNECTION_TIMEOUT`, ten seconds), and the live tier waits for that rather than asserting the slice took it.
- The next step's slice **MUST NOT** be created until the previous one reports inactive. `EndStep(n)` does not return until its stop completes, so the run loop's next `BeginStep(n+1)` follows an inactive slice by construction.
- When a step closes on evidence, mill **MUST** take a read-only snapshot of each of the run's cache copies before stopping the slice. That snapshot is the promotion candidate; a later evidence-closed step replaces it. A step that is cancelled, times out, or dies leaves no candidate. `EndStep(n, true)` takes the candidate before it stops the slice; `EndStep(n, false)` takes none. For a workspace with a step boundary the candidate is `EndStep`'s, and the run loop's own cache-candidate hook stands aside.
- A `podman run` from the step's shell lands under the step's slice only with `--cgroups=split`, which the base image's `containers.conf` pins; without it, it lands under `user@1000.service` and survives the slice stop. `podman ps` is not evidence after a kill. A workflow that wants a container to outlive a step has misdesigned the step. That a container started in step 1 lands under `mill-step-1.slice` — and so is reaped with it — is checkable on the host with `systemd-cgls -M mill-<id>`, which the live tier asserts.

### Destroy

- **Never before ready.** A stop that lands before the machine's init has installed its signal handlers is lost: nspawn waits for a shutdown that never begins, the unit hangs for its whole stop timeout, and the payload survives (the spike's E0 addendum: a stop within ~200 ms of start). A machine that never reported ready — the readiness timeout's path — is not stopped; it is terminated (below), which ends a mid-boot machine in about a second with nothing left behind. `runs/<id>/run.json` records when the machine reported ready, so a worker that attaches knows which path applies.
- **After ready**, `systemctl stop mill-machine@<id>.service` with a bound (`StopTimeout`, 90 s unless configured, above the unit's own `TimeoutStopSec=60`). The unit's `ExecStop` is the helper's `shutdown`: one SIGTERM asks nspawn to halt the machine, a quarter of a second once booted; a booted machine that will not halt gets nspawn's second SIGTERM after 20 s, which kills the machine and removes its snapshot and mount points itself. `KillMode=mixed` covers the rest. A SIGKILLed nspawn leaks the ephemeral snapshot, which is why the unit's final signal is SIGTERM and not SIGKILL.
- **Then the truth from machined.** If `machinectl show mill-<id>` still knows the machine after the stop, the worker runs the helper's `terminate mill-<id>` — `machinectl terminate`, which under `--keep-unit` is only machined asking the unit to stop and one more signal init may lose, followed after a second by nspawn's second SIGTERM and then SIGKILL — and asks again. Only if the machine is registered after that does `Destroy` return an error naming it; a run **MUST NOT** be marked finished while its machine is registered, and the run directory stays with it for reconciliation.
- Then promotion: the candidate from the last evidence-closed step becomes each lineage's next generation (§Promote), or nothing is promoted. Then the run directory — workspace, `machine.json`, `run.json`, the proxy file, the cache copies and candidates — is removed, a cache copy by `btrfs subvolume delete` through the driver as the worker's uid, and the socket directory under `/run/mill` with it. The ephemeral root is nspawn's to discard and is gone with the unit.

## Filesystem

| Path in machine | Backing | Mode | Lifetime |
|---|---|---|---|
| `/` | ephemeral snapshot of the image subvolume | rw | machine |
| `/work` | `runs/<id>/workspace` on host | rw | run; mill deletes it |
| `/run/mill` | `/run/mill/<id>` on host | socket only | run |
| `/home/agent/.claude/skills/mill` | worker's skill dir (`skill-dir`, `~/.local/share/mill/skill` by default; the helper's policy admits the share root) | ro | worker version |
| `/home/agent/.local/share/mill/sidecar` | worker's copy of the built sidecar (`sidecar-dir`) | ro | worker version |
| `/home/agent/.local/share/mill/workflow` | worker's copy of the workflow directory it loaded (`workflow-copy-dir`); absent when it loaded none | ro | worker preflight |
| cache paths | `runs/<id>/cache/<name>`, a writable snapshot of a generation | rw | run; promoted or deleted |
| `/etc/environment.d/50-mill-proxy.conf` | `runs/<id>/environment.d/50-mill-proxy.conf` on host | ro | run |
| `/var/lib/machines` | the host's image subvolumes; `nested` only | ro | run |
| `/var/lib/mill/dagger` | the host's dagger checkout (`dagger_checkout`) for the test fixture, `MILL_DAGGER_REPO` inside; `nested` only | ro | run |
| `/.hostsys` | the host's `sysfs`, by the helper; `nested` only | ro | machine |
| `/var/lib/containers` | ephemeral root | rw | machine |

- The workspace is bind-mounted rather than cloned inside so the host can read evidence from it and keep it for a post-mortem when a run fails, and so the git and forge verbs run on the host against it with the key and the credential that never enter the machine (§Verbs). The bind option `idmap` is what makes uid 1000 inside uid 1000 on the host — `--private-users-ownership=auto` idmaps the root tree only (the spike's E4) — and the helper puts it on every bind. A commit a session makes inside on its own is attributed to the mill's identity and unsigned; the push verb refuses it (§Verbs). A cache copy is therefore created as the worker's uid and never through `sudo`: a copy made as root is one `agent` cannot write, which is why the cache store has a driver of its own without `Commands.Sudo` while the image store's has it (§Storage).
- Container storage is ephemeral. Podman's storage carries a database and locks and is not safely snapshottable under a running daemon; image pulls are made cheap by Harbor's proxy cache instead (§Network).
- No other host path is mounted. Not `/dev/kvm`, not the host's podman socket, not the operator's `$HOME`, not the mill binary. The helper refuses a source outside the run directory, `/run/mill/<id>` and its policy's read-only sources, so the list above is enforced and not merely intended.
- `/run/mill` on the host is the host's to create, owned by the worker's account; the worker cannot make a directory under `/run` and `Check` refuses before a claim when it is missing.

## Storage

`core`'s root is XFS (§The host), so the sandbox gets a volume of its own: `nvme1n1`, LUKS with a keyfile on the encrypted root so it opens after root does, btrfs on top, mounted at `/var/lib/mill` with `user_subvol_rm_allowed`, and a `machines` subvolume bind-mounted at `/var/lib/machines`. Lineages, run directories, image subvolumes and squid's cache all live there. This is homestead's `mill_storage` role and the first prerequisite of the spike; nothing below runs on XFS as written. XFS reflink would have carried the design at file-level copy-on-write — seconds rather than milliseconds to restore a large cache — and is the fallback if the disk ever has to go back.

## Caching

A cache is a lineage: a directory whose state is carried between runs as btrfs snapshots. No keys are needed to find one; the lineage's name and scope are its identity, and the latest generation is what restores.

### Kinds and scope

| Kind | Examples | Verified by | Scope |
|---|---|---|---|
| toolchain | mise's data dir | mise's checksums; version dirs are immutable | `global` |
| dependency | `GOMODCACHE`, cargo registry, npm/bun cache, gem cache | the package manager, against the lockfile | `global` |
| build state | `GOCACHE`, `target/`, `node_modules`, `.bundle`, `tmp/cache` | nothing | `repo/<repo-id>` |

Global lineages are shared by every run on the worker because their contents are verified by construction: a corrupt entry fails verification and is refetched. Build state is trusted only from a step that closed on evidence, and only within its repo.

### Defaults

Derived from the tools `mise.toml` declares. Names are stable so `mill.toml` can override or disable them.

| mise tool | lineage | path | scope |
|---|---|---|---|
| any | `mise` | `~/.local/share/mise` | global |
| go | `go-mod` | `~/go/pkg/mod` | global |
| go | `go-build` | `~/.cache/go-build` | repo |
| rust | `cargo-registry` | `~/.cargo/registry` | global |
| rust | `target` | `/work/target` | repo |
| node, bun | `npm-cache` / `bun-cache` | `~/.npm`, `~/.bun/install/cache` | global |
| node, bun | `node_modules` | `/work/node_modules` | repo |
| ruby | `gem-cache` | `~/.gem` | global |
| ruby | `bundle` | `/work/vendor/bundle` | repo |

Paths are the tools' defaults; a repo that relocates one through `mise.toml`'s `[env]` overrides the lineage's path in `mill.toml`.

### Restore

For each lineage the run uses: `btrfs subvolume snapshot <lineage>/gen-<latest> runs/<id>/cache/<name>`, then bind into the machine at the lineage's path. An absent lineage, or a `key` mismatch, restores an empty subvolume. Restore is O(1) regardless of size; that is the reason the store is btrfs and not an archive.

### Promote

- At the end of a run, the candidate snapshots from the **last evidence-closed step** become the next generation of each lineage: rename into `gen-<n+1>`, update `manifest.json` (created-at, size estimate from `du -s` of the read-only snapshot, producing run and step, key hash if any), and prune the lineage to its two newest generations.
- A run with no evidence-closed step promotes nothing.
- A lineage whose global-scope cap would be exceeded by promotion is **reset**: the promotion is discarded and the lineage's generations deleted, so the next run starts it empty and pays one reinstall. Global lineages are content-addressed; a reset costs time, never correctness.
- Concurrent runs of one repo each promote a generation; the lineage is a sequence, and the later promotion wins. Neither run's writable copy is affected by the other.
- **A nested run never promotes into the outer's lineages.** An inner acceptance run's steps close on evidence too, and a lineage bind-mounted from the outer would receive a generation produced by a test. When `[machine].nested` is granted, the inner worker is given a lineage root of its own under the outer's ephemeral root, and the outer's caches are mounted into the outer machine only; an inner promotion that somehow addresses an outer lineage is refused.
- The manifest is operational state of the worker, not the ledger. The ledger records only that a run restored and promoted which lineages.

### Decided

What the rules above leave open, `internal/workspace/machine/cache` decides as follows; every subvolume operation goes through the `Driver`, as the worker's uid, never through `sudo`.

- **Repo id.** `repo/<repo-id>` uses `RepoID(origin)`: the origin with its scheme, user and `.git` dropped and every character outside `[A-Za-z0-9._]` replaced by `-`, then `-` and the first eight hex characters of the origin's sha256 — `gitlab.com-ivy-mill-1a2b3c4d`. Readable in `mill cache ls`, path-safe, and two origins that slug alike stay apart. Lineage names and repo ids must match `^[A-Za-z0-9][A-Za-z0-9._-]*$`; a lineage that does not is refused before anything is snapshotted.
- **The run's record.** `runs/<id>/cache.json` is the run's own note: the lineage root it restored from, each lineage with the generation it restored (or why it started empty: `no generation`, `key changed`), and the step whose candidate it holds. `Promote` reads it. The ledger duplicates none of it; what it is handed is the `Restored` and `Promoted` values.
- **Candidates** live at `runs/<id>/candidate/<name>`, read-only. A new candidate is taken whole into `candidate.next` before the previous step's is deleted, so a failure part way leaves the previous candidate standing. A candidate that cannot be taken is a warning in the worker's log, never a failed step: the evidence is recorded and the run promotes from the last candidate it did take, or nothing.
- **Promotion is a rename.** `rename(2)` refuses a read-only subvolume's root with `EROFS`, as `unlink` does, so the candidate's `ro` is cleared, it is renamed into `gen-<n+1>`, and `ro` is set again: the generation is the candidate's own subvolume, not a copy. The size estimate is `du -s` of the candidate once, before the rename. Generations are numbered from the directory tree — the highest `gen-<n>` present plus one — under a `flock` on `<lineage>/lock` that a restore holds too, so two runs promoting at once take consecutive numbers and a restore never snapshots a generation being pruned.
- **The manifest** is reconciled with the tree on every read: a `gen-<n>` the tree has and the manifest does not is listed by number alone, and one the manifest has and the tree does not is dropped. The manifest is written before pruning and again after it. A lineage gets its directory and manifest at its first restore, so `mill cache ls` shows a lineage that restores empty every run, with when it was last restored.
- **Caps.** The global cap is worker configuration — `cache-global-cap`, 20 GiB unless set — compared against the candidate's estimate; a reset deletes the candidate and every generation and records `reset` (when, which run and step, the estimate, the cap, one sentence) in the manifest, which `mill cache ls` shows as the `reset` flag until the next promotion. The repo cap — `cache-repo-cap`, 10 GiB unless set — is only the `over-cap` flag.
- **Outside the root.** `Promote` compares the record's lineage root with the worker's own and refuses any other with `ErrOutsideRoot`; a nested worker's record names its own root, so its promotion never reaches the outer's lineages.
- **Discard.** Removing the run's copies and candidates is `Discard(run)`, the cache half of removing the run directory at destroy; it is called after `Promote`, or instead of it when the run promotes nothing.

## Eviction

Three mechanisms, applied in this order at each trigger.

1. **Within a lineage**: keep the two newest generations. Applied at promotion.
2. **Expiry**: a repo-scoped lineage or an image subvolume not restored or booted for 30 days (worker config) is deleted.
3. **Watermarks**: when free space on the cache filesystem — `statfs`, the truth — is below the high watermark, delete repo-scoped lineages and image subvolumes in order of least recent use until free space exceeds the low watermark. Two thresholds so eviction does not run at every promotion. Global lineages are not evicted by watermark; they are bounded by their caps (§Promote).

- The sequence is delete → `btrfs filesystem sync <mount>` → `statfs`. Deletion frees space asynchronously, and an eviction loop that does not wait over-evicts. `btrfs subvolume sync` needs root and does not move `statfs` anyway; `filesystem sync` is the unprivileged commit that does (the spike's E9). The driver's `DeleteSubvolume` takes a `sync` flag for it, and eviction sets it on every deletion. A read-only generation has its `ro` property cleared before the delete.
- Do not enable qgroups for this. Per-lineage sizes are estimates for ranking victims; free space decides whether to evict. `du -s` overstates on-disk use about 2:1 on a `compress=zstd:1` volume, so the watermarks read `statfs` and the size `mill cache ls` shows is the manifest's estimate, never a fresh `du`.
- A lineage under eviction may be the parent of an in-flight run's writable snapshot. That is safe: btrfs snapshots are independent. A run whose lineage was evicted mid-run promotes into a fresh lineage.
- Triggers: after each promotion, the watermark pass alone — the cheap check; at worker start, after reconciliation, both passes; and `mill cache prune`, both passes, which `contrib/systemd/mill-cache-prune.timer` runs daily so a host with no runs still expires what it holds.
- A per-lineage cap on repo scope is a number `mill cache ls` flags, not a gate on promotion. A project whose build state is naturally large is still cached; the watermark decides.
- Operator tools: `mill cache ls`, `mill cache prune`, `mill image ls`, `mill image prune`, all in `docs/tools.md`. `image prune` is the same eviction with images as the only victims. `mill doctor`'s `space` probe reads free space against the watermarks.
- Not built: consulting dagger's ready set so a repo with pending work is protected from eviction. Least recent use alone ranks the victims.

### Decided

What the rules above leave open, `internal/workspace/machine/cache`'s `Evict` decides as follows.

- **Policy.** Worker configuration: `cache-expiry` (a window, 30 days unless set) for repo lineages, `image-expiry` for images when it differs; `cache-high-watermark` and `cache-low-watermark`, each a share of the volume such as `10%` or a size such as `50G` — `10%` and `20%` unless set. Eviction starts when free space is below the high watermark and stops once it is above the low one, so the low watermark is the larger amount of free space: the names say how full the volume is, the numbers say what is free. A low watermark below the high one is refused. `cache-global-caps` names a cap per global lineage, over `cache-global-cap` for the ones named.
- **Last use.** A lineage's is the later of its last restore and its latest generation's promotion; an image's is its last boot, or its build when nothing has booted it. Victims are ranked ascending on it, lineages and images together, whole lineages at a time.
- **Victims.** Repo-scoped lineages with at least one generation, and image subvolumes. Never a global lineage, in either pass; never anything under `runs/<id>`. A lineage's eviction holds its lock, so a restore or promotion in flight waits and sees the tree as it is.
- **What stays.** An evicted lineage keeps its directory, lock and manifest — the manifest with no generations and an `evicted` note (when, which generations, why), which `mill cache ls` shows as the `evicted` flag until the next promotion clears it. Deleting the directory would leave a run waiting on the lock with no directory to promote into; a fresh lineage is `gen-1` in the directory that is there.
- **The report.** Every deletion names its kind, the lineage or image, when it was last used, which pass took it and one sentence why; then free space against the watermarks. The worker prints it at start, `mill cache prune` and `mill image prune` print it, and the promotion trigger logs it. Eviction that fails part way is a log line at the promotion trigger and at start, never a failed run or a worker that does not start.

## Network

- The machine has a private network namespace on a host-side veth named `ve-mill-<run-id>` (truncated to the interface-name limit; mill **MUST** record the actual name). The host addresses it the way systemd's own `80-container-ve.network` does — a DHCP server on the host side of every `ve-mill*` link, no masquerade — with NetworkManager told to leave `ve-mill*` unmanaged.
- The host's nftables table is **static**, because the worker is unprivileged and cannot add rules at run time: one `table inet mill` matching `iifname "ve-mill*"`, permitting DNS to the host resolver, TCP to the proxy and gateway ports on the host, and established/related; dropping everything else, including forwarding between `ve-mill*` links and to the LAN. Its own table and its own loader unit, never a `flush ruleset`, because firewalld owns `inet firewalld` on `core` (the pattern homestead's `thicket_nftables` role already uses).
- Egress is a forward proxy on the host — squid — at `http://10.99.255.1:3128` on the `mill-host` dummy link homestead's `mill_egress` role creates (`homestead#34`), reachable from every machine whatever /28 it was given; the worker's `proxy` and `no-proxy` keys carry that and `localhost,127.0.0.1` as defaults, and the gateway's host is always added to `NO_PROXY`: the gateway is on the host side of the machine's own link and admitted there directly, and a request for it sent through squid arrives as a host-bound connection squid refuses. It is **transparent in policy**: no allowlist, no interception, `CONNECT` passed through so the TLS a session sees is the origin's. Its job is to be the only path out, so the network architecture is settled and a smarter proxy can replace it without the machines noticing. Squid admits the machines' pool addresses and refuses host processes: a probe run *on the host* with `--interface 10.99.255.1` is denied like any other host process, while a request from inside a machine is `TCP_TUNNEL` 200 at both `10.99.255.1:3128` and the veth's own `.1` (`homestead#40`). `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` are set in every exec's environment — and in `agent`'s user manager from boot, because podman's API service pulls with *its* environment, not the exec's (the spike's E6): `Create` writes `runs/<id>/environment.d/50-mill-proxy.conf` with both cases of the three names and `ANTHROPIC_BASE_URL` at the gateway, and binds it read-only at `/etc/environment.d/50-mill-proxy.conf` in `machine.json`.
- The nftables drop is what makes the proxy mandatory, not the proxy itself. A session that ignores the environment and connects directly gets a timeout, not a bypass.
- **The proxy variables MUST cross both environment floors.** The harness adapter (`envFloor`, `internal/harness/claude/environment.go`, which is `workspace.FloorNames`) and the sidecar (`ENVIRONMENT_PASSTHROUGH`, `sidecar/src/session.ts`) are allowlists of eleven names, and the SDK does not merge an exec's environment with them. Setting the variables on the exec alone leaves a session's shell with no proxy on a host that drops the direct path: every outbound call hangs to its timeout with nothing naming a proxy. They are copied past both floors the way `mill#50` copies the workspace-git variables — as decisions the workspace made, not values the host supplied — in both files, which already point at each other.
- The session **MUST** be told. The mill skill and the session's system prompt (set by the harness adapter) state that all egress goes through the proxy named in the environment, so an agent debugging a failed connection reaches for the right explanation.
- Per-workflow allowlists are a later addition to the proxy's configuration, not to the machine's. When they arrive, the workflow's declaration — the model API, the forge, Harbor, distro mirrors — becomes the proxy's rule set, and nothing else in this section changes.
- **Container images come through Harbor's proxy-cache projects.** The base image's `registries.conf` rewrites `docker.io`, `quay.io`, `ghcr.io` to the corresponding Harbor project. Nothing pulls from an upstream registry directly, and the machine's container store can stay ephemeral because a second pull of the same image is a LAN fetch.
- RPM repositories are not proxied by Harbor; `dnf` reaches the mirrors through the proxy. Fedora's mirrors are HTTPS, so squid's own cache does not help without interception; a `dnf` cache on the worker is the fix if it proves noisy.
- Containers the test suite starts inside the machine publish ports on the machine's loopback, which is what testcontainers expects. They are not reachable from the host.

## Credentials

- The store issues a token per step. It is the session's only credential and the one thing that does enter the machine — as `STEP_TOKEN` for the socket's verbs and `ANTHROPIC_AUTH_TOKEN` for the gateway — and it is worth nothing outside: the socket and the gateway are reachable from the machine alone, and `Rotate` at the step boundary retires it. Everything the token stands for — the forge credential, the signing key, the subscription — stays on the host side of the socket.
- A session needing a derived credential — git, forge, registry — asks over the socket; the host mints it against the step's token. Derived credentials **MUST** be short-lived and **MUST** be revoked when the step slice stops.
- Git credentials never enter the machine: the verbs that need one — `git.push`, `forge.pr`, `forge.checks` — run on the host against the workspace bind (§Verbs), and a session's own `git push` inside has none. No token is written under `/work` or `/home/agent`.
- **The model credential never enters the machine.** A session on the account's login reads `~/.claude/.credentials.json`, which is the account's OAuth *refresh* token — the one credential on the box that outlives every step and that a session's shell can read. In a machine there is no such file. Instead the session holds the step token and the host holds the subscription, with the **credential gateway** (`internal/gateway`) between them:
  - every exec sets `ANTHROPIC_BASE_URL` to the gateway on the host side of the veth, beside squid, and `ANTHROPIC_AUTH_TOKEN` to the **step token** — copied past both environment floors as a workspace decision, like the proxy variables (`workspace.Egress`). Claude Code sends `ANTHROPIC_AUTH_TOKEN` as `Authorization: Bearer` above every file-based credential, so a session with no login and that variable set authenticates through the gateway alone. `ENABLE_TOOL_SEARCH=true` goes with them, because the CLI turns its MCP tool search off when the base URL is not a first-party host and the gateway forwards bodies unchanged (spike §E14);
  - the gateway listens where configuration says — `gateway-listen`, default `10.99.255.1:8080`, the `mill-host` dummy link homestead's `mill_egress` role creates and the one port besides squid's that the `mill` firewalld zone and the `inet mill` table admit from `ve-mill*` — and is turned on by `gateway: true`; off, a run behaves as it always has, on the account's login, which is what a laptop `mill run` gets. The local backend passes the same two variables when it is on;
  - it forwards the Messages API — `POST /v1/messages` and `/v1/messages/count_tokens` — and answers 404 to everything else. It checks the bearer against the token the run loop last rotated in for a live attempt, by hash, exactly as the workspace socket does, and refuses anything else with 401: `Rotate` at the step boundary is what retires the previous step's token. It swaps the bearer for the subscription credential, rewrites `Host`, drops the hop-by-hop headers, and forwards the rest of the CLI's headers — `anthropic-beta`, `user-agent`, `anthropic-version` — unchanged, which is all the upstream wanted (spike §E14). Streamed responses are flushed as they arrive; a client that abandons a stream, as the CLI's parallel title request does, ends that response and nothing else;
  - the upstream credential is a `claude setup-token` — subscription-billed, one year, interactive once — read from `$CREDENTIALS_DIRECTORY/claude-oauth`, delivered to the worker by `LoadCredential` from `vault_claude_code_oauth_token`, or from the file `model-credential-file` names. Never from the environment, and never logged: the log names a step token by its id and the credential by a fingerprint of its hash;
  - the forge token takes the same path: `$CREDENTIALS_DIRECTORY/forge-token`, delivered by `ImportCredential=forge-token` from `/etc/credstore` or by `LoadCredential`, and read before `$MILL_FORGE_TOKEN`, which stays the fallback for a laptop and for a deployment that still sets it in `/etc/mill/env` (`docs/tools.md` §Forges);
  - the gateway records every response's `usage` block as a ledger row `(step run, model, input, output, cache read, cache write)` with source `gateway`, distinct from the adapter's row with source `sdk` for the same turn. The adapter's rows are the step's cost — what `mill status` shows and what is posted to dagger — and the gateway's sit beside them: `mill inspect` carries them as the attempt's `gateway_totals`, and `mill cost` lists both, one row per source;
  - a step whose workflow declares `token_budget` — every token the attempt bills through the gateway, input, output, cache read and cache write summed — is refused with 429 and a body naming the budget once the attempt has spent it; the run loop reads the refusal when the session ends and fails the attempt on it, whatever the session made of being refused, so the budget is the failure the ledger, the retry prompt and the park gate carry. A step whose workflow declares `models` is refused 400 for a request naming a model outside the pin; an entry matches a model id it equals or that contains it, so `sonnet` pins every Sonnet — and a pin has to admit the auxiliary model the CLI bills beside the one the step asked for. The budget is per attempt: a retry starts a fresh one, and `retries` bounds how many;
  - `mill doctor`'s `gateway` probe makes one token count through the gateway — a request the upstream answers without billing — and fails when the credential file is absent or the upstream refuses it; it warns at eleven months from `model_credential_minted` in the configuration file, because the CLI's three-day expiry warning applies only to `/login` credentials (docs/tools.md §doctor).
- **Neither the production dagger nor the production mill socket ever reaches a session.** `DG_SOCKET` and `MILL_SOCKET` name the CLI's and the sidecar's own sockets on the host and, unguarded, would ride into a session's environment on the same path as every other inherited variable. Both backends overwrite them for every exec with two paths under the run's own directory — `workspace.DeadSockets` — that nothing listens on: a session that dials either fails exactly as it would against a socket that had crashed, not against one it was never meant to see. In a machine this is belt and braces over the bind list, which never mounts anything at `/run/dagger` or names the host's mill socket in the first place; on the local backend it is the only guard there is, since the child shares the host's filesystem.
- `HOME` stays on the environment floor for the shell, transcripts and the harness's own state — not for a login; `envFloor` and `ENVIRONMENT_PASSTHROUGH` say so.
- The clone at step 2 of the sequence uses the worker's forge credential on the host. No session is running; no session credential exists yet.

## Session identity

- Sessions run as `agent`, uid 1000, home `/home/agent`, not root. `--private-users=pick` maps uid 1000 into the machine's range; the host sees an unprivileged uid.
- `/work` is owned by `agent`. Commit identity and signing come from the workspace (#50), not from the session's shell: the identity as the exec's `GIT_AUTHOR_*`/`GIT_COMMITTER_*`, the signature from the `git.commit` verb on the host (§Verbs).
- `/home/agent` is on the ephemeral root. Everything under it dies with the run except the cache lineages mounted into it.
- `~/.claude` is ephemeral, with the skill mounted read-only into it. The harness's own scratch under `~/.claude` is written and lost.
- Root exists for systemd and for the `dnf` shim. Nothing else escalates.
- A fixed uid rather than per-run: paths in documentation and `mill.toml` stay literal, and isolation does not depend on the uid — the user namespace is the boundary.

## Skill mount

- The mill skill and the sidecar are the worker's, versioned with the binary, and **MUST NOT** be baked into the image. The worker bind-mounts the skill read-only at `/home/agent/.claude/skills/mill` at provision; the sidecar is placed where the harness adapter expects it, by the same mechanism.
- **What is mounted.** The skill is embedded in the binary (`plugin.go` at the repository root embeds `plugin/mill/skills/mill`), and the worker writes it to its skill directory — `skill-dir`, default `~/.local/share/mill/skill`, the one read-only source homestead's `mill-machine` policy admits — at its preflight, before any node is claimed (`internal/skill`, `checkWorkspace` in `internal/cli/run.go`). Beside `SKILL.md` and its `references/` it writes `VERSION`: one line, the worker's version as `mill version` prints it, the breadcrumb principle 6 allows and the only thing in the directory the plugin does not ship. Each file lands by rename and an unchanged file keeps its inode, so a machine that has the directory bound while a newer worker rewrites it reads whole files; whatever else an earlier worker left there is removed. The directory is bound read-only at `/home/agent/.claude/skills/mill` in every `machine.json` (§Provision).
- **Where the rest of `~/.claude` stands.** It is the session's to write: the harness's settings, transcripts and scratch go under it and are lost with the run (§Session identity). A mount point nspawn has to make is root's, so the base image makes `/home/agent/.claude/skills/mill` as `agent`'s ahead of the bind (`images/base/Containerfile`); with the directory root-owned every write to `~/.claude` is refused. Inside a machine, a write under `~/.claude/skills/mill` fails with a read-only filesystem error and `~/.claude/settings.local.json` can be written; the live tier checks both, and that `VERSION` matches `mill version`.
- **The sidecar rides in beside the skill.** It is the worker's too, versioned with the binary, and **MUST NOT** be baked into the image — but it cannot be embedded the way the skill is, because its `node_modules` is far larger than a binary should carry. The worker copies the built sidecar — its `dist` and `node_modules` and nothing else, pnpm's relative links carried as links so the tree resolves the same inside the mount, file modes kept so `.bin` shims stay executable — from where it keeps it into `~/.local/share/mill/sidecar` at its preflight, beside the skill and under the same share root the helper's policy admits (`homestead#42`), with `VERSION` beside it. A source with no `dist/main.js` is not a built sidecar and is refused before anything is written; a link that leaves the source is refused the same way. The copy is gated on that breadcrumb: a directory already at the binary's version is left untouched, so an unchanged worker copies nothing; a differing version refreshes the whole tree by rename, each file landing atomically, and prunes what an earlier version left. The directory is bound **read-only** at `/home/agent/.local/share/mill/sidecar` in every `machine.json` (§Provision), the mount point the base image owns as `agent`'s ahead of the bind (`images/base/Containerfile`), exactly as the skill's is.
- **The workflow directory rides in the same way.** A check step names its script relative to the workflow it was loaded with (`scripts/clean-tree`), the store keeps the script's digest and not its bytes, and the worker resolves the name under `--workflow-dir` on the host — a path the machine does not have. So the worker copies the workflow directory it loaded from into `~/.local/share/mill/workflow` (`workflow-copy-dir`) at its preflight, links resolved rather than carried — the acceptance suite stages a derived workflow as links into the tree, which would name nothing inside a machine — and file modes kept so a script stays executable; there is no version to gate on, the directory being the operator's rather than the binary's, so every preflight walks it and prunes what an earlier one left. It is bound **read-only** at `/home/agent/.local/share/mill/workflow` in every `machine.json`, and `Exec` names a program the worker resolved under the loaded directory by its path under that mount, leaving every other path and every argument as given. A worker that loaded no workflow directory binds nothing.
- **The session is one exec like any other.** When the backend is `machine`, the harness adapter's execer is the run's machine workspace (`internal/cli`, `claude.WithExecer`), so a skill step's sidecar enters the machine through the helper under the step's slice, as `agent`, in `/work` — reaped when the step ends and gone with the machine, like every other exec (§Exec, §"Step boundary"). Its entrypoint is the in-machine node running the mounted sidecar: `/usr/local/bin/node /home/agent/.local/share/mill/sidecar/dist/main.js`, node named by its absolute path so the entrypoint does not lean on the machine's `PATH`. The socket the adapter hands it is the socket's machine-side mount, `/run/mill/ws.sock` (§Naming), not the host path the worker serves on; and because the machine supplies `agent`'s `HOME`, `PATH` and the rest, the adapter copies none of the host's environment in — only the session's socket and step token, the workspace's egress and identity, and whatever the step's `env` named. The local backend is unchanged: its sidecar runs on the host at the configured entrypoint.
- The skill **MUST** document `mill.toml`: the `[image]` and `[cache.<name>]` tables, the path rules, the defaults by ecosystem, and that runtime `dnf` installs are not remembered. A spec holds it to the parser: every key `config.Load` accepts (`config.Keys`) appears in the skill's text.
- The skill and the session's system prompt **MUST** both say how the session reaches out (§Network). The adapter appends one paragraph to the harness's own system prompt — every outbound connection goes through the proxy named by `HTTPS_PROXY`/`HTTP_PROXY`, a direct connection times out by design, the model is reached through `ANTHROPIC_BASE_URL` with the step token as bearer — composed from the workspace's decisions and only when it made them: it crosses the wire as `system_prompt` on `Start` and `Resume` (`proto/harness/v1`), and the sidecar appends it to the SDK's `claude_code` preset. The local backend without a proxy adds nothing.

## The dnf shim

- `/usr/local/bin/dnf` in the base image (`images/base/dnf`) wraps `sudo /usr/bin/dnf`, and the sudoers rule allows `agent` to run `/usr/bin/dnf install` and nothing else. The proxy goes to dnf as `--setopt=proxy=` after `install`, because sudo strips the environment and the rule's pattern is `dnf install *`.
- On success the shim prints, as its **last lines**, a hint: the packages installed, that this machine is ephemeral, and the `[image].packages` line to add to `mill.toml`. Agents read the tail of output; the hint is where they look.
- The shim reports the install over the workspace socket: a POST to the `report.install` verb at `/run/mill/ws.sock`, the socket directory bound `:idmap` at boot like every other bind, with the step token the sidecar put in the session's environment (`STEP_TOKEN`) as the bearer every verb wants. No socket, no token, or a socket that does not answer the verb costs nothing; the hint is still the last thing the agent reads. The workspace holds the report (`workspace.Reporter`) and the run loop drains it into the run's provenance after the attempt: an `installed` event in the ledger, "installed `<packages>`, not declared in `mill.toml`", against the attempt whose session made it, which `mill history` shows. This is not routed to the reviewer, who judges acceptance criteria only; it is for the operator to see which projects keep needing something undeclared.
- Nothing installed this way survives the run. The declaration the agent commits is what is remembered.

## Preparatory steps

A preparatory step readies the machine for the sessions. It is a deterministic step with an exit code and output as evidence, and it has a time budget. Its output is recorded, not shown to a session, which keeps `mise install`'s pages of progress out of the session's context.

- **Image build** (§Derived images) — runs before boot. Failure → boot from base, finding to the first session.
- **Toolchain** — `mise trust /work && mise install`, first exec in the machine. Failure → the run continues into the first session with the last lines of output as a finding. The agent fixes `mise.toml`; the next run's toolchain step passes.
- **`mill.toml` validation** (§mill.toml) — failure → defaults, finding.

The rule: a preparatory step **MUST NOT** fail the run. The circuit breaker exists for work that cannot be done, not for a typo an agent can fix in one session.

The step kind, exactly (`kind: preparatory`, `docs/domain-model.md` §Step; `internal/run/preparatory.go`):

- **Declaration.** A workflow step naming a command under `check.command` and closing on `exit`, with an explicit `timeout`. The loader refuses `exit`, `stdout` and `feedback` on it — those are a check's contract, and a preparatory step has none — and refuses a session, a skill or a gate on it. `workflows/quick-fix` and `workflows/feature` open with `toolchain`: `sh -c 'mise trust /work && mise install'`, `timeout: 20m`. `mise trust` is pre-granted for `/work` in the base image; the step runs it anyway so a repo whose config moved is trusted too.
- **Execution.** The command runs through `Workspace.Exec` — in a machine, as `agent`, under the step's slice — with its own step token rotated in like any attempt, under the step's `timeout` and nothing else. Both streams are captured whole; at the budget the process is killed and the exit is recorded as `-1` with reason `timeout`.
- **Evidence.** The step closes on `exit` evidence carrying the process exit and the captured output, verified by `preparatory <name>`, **whichever way the command ended**: a non-zero exit and a spent budget close it exactly as a zero does. The record of the run is the fact. `mill logs <run> --step <name>` reads the same output from the step's log.
- **Findings.** A non-zero exit, or a spent budget, owes the first session a finding: the step's name and command, how it ended (`exited 3`, or `was stopped at its 20m0s budget`), and the last twenty lines of its output. It is derived from the evidence when the first session is prompted, not remembered by the worker, so a worker that dies between the step and the session delivers it anyway. Delivery is the path a reviewer's `revise` findings already take — the feedback the prompt appends under its one heading — and provisioning's own findings (a derived build that fell back to base, an invalid `mill.toml`, `workspace.Provisioned`) take it too, each as one line prefixed `Provisioning:`. They are delivered to the first skill step until any session in the ledger has started; after that, nothing owes them.
- **Never a failure.** The step is closed, not failed: no retry is consumed, `on_failure` is never consulted, and the breaker cannot trip on it — so the loader refuses `retries` and `on_failure` on one, and `mill workflow show` prints neither. A step that exited 0 owes nothing and says nothing.
- **Only where there is a machine.** The run loop runs a preparatory step only in a workspace that provisions one (`workspace.Provisioned`: the machine backend). The local backend is the operator's own host, where `mise trust /work` would trust whatever `/work` resolves to on the host; there the step closes on `exit` evidence saying it did not run (`-1`, reason `refused`, the output naming why), and that is not a finding.

Budgets are the workflow's. A preparatory step and a check step each run under the `timeout` the workflow definition gives them — `workflows/feature/workflow.yaml` already carries `20m`, `30m` and `168h` on its steps — not under any harness setting. Two consequences:

- Every preparatory step **MUST** have an explicit `timeout`; there is no default that is right for both a warm `mise install` and a cold Rust toolchain.
- **The sidecar MUST set the session's shell timeout to the step's budget.** It does, from the `timeout_ms` the adapter puts on every `Start` and `Resume` (`docs/architecture.md` §"Skill steps: the harness adapter"). Without it a session gets the SDK's default and a command over about two minutes is backgrounded — after which a session invents a poll loop and loses an hour, which is the failure the harness fixed for its own sessions (`mill#42`) before mill's. A session that can wait in the foreground never has a task to lose.

## Resource limits

- `MemoryMax`, `CPUQuota`, `TasksMax` on the machine unit bound the whole run, containers included, because the machine's cgroup tree is delegated from that unit.
- A workflow **MAY** declare per-step limits, set on the step slice.
- Disk is bounded by eviction, not by quota, in the first iteration. A run that fills the filesystem is the failure mode this leaves open.

## Privilege

The worker is not root. It runs as the operator's account, as a user unit — `contrib/systemd/mill.service`, deployed by homestead — because that account is mill's dagger principal and holds the forge token and the model credential, and none of that should be root. What needs root is starting a machine and entering it, and both are separated out the way `homestead-apply` already is: root-owned pieces the operator's uid may drive, each admitting exactly what it names. The spike's E0 decided the shape; homestead's `mill_machine` role (`homestead#33`) deployed it.

- **Start and stop are `systemctl` under polkit.** `mill-machine@.service` is a root-owned template on the system manager. Its `ExecStart` is the helper `/usr/libexec/mill-machine %i`, which validates `/var/lib/mill/runs/%i/machine.json` and execs the `systemd-nspawn` invocation of §Provision. The worker starts and stops it with `systemctl start|stop mill-machine@<id>` under `/etc/polkit-1/rules.d/50-mill-machine.rules`, which grants its uid `org.freedesktop.systemd1.manage-units` for `^mill-machine@.*\.service$` and nothing else, plus machined's `shell` and `manage-machines`. A unit outside the template — a new name or an existing one — is refused with polkit's interactive-auth prompt.
- **Everything that enters a running machine is the helper under sudo.** `systemd-run -M` and `systemctl -M` as the operator fail with `Failed to open namespace of PID …: Permission denied` regardless of polkit, and `machinectl shell`, the one polkit-mediated entry, returns 0 for an exit status of 42 and merges stderr (the spike's E0). So exec, a stop inside the machine, and terminate are `sudo -n /usr/libexec/mill-machine exec|stop|terminate`, admitted by `/etc/sudoers.d/mill-machine` for those three verbs, `NOPASSWD`, no tty. The helper validates before anything privileged runs: `exec` admits `Slice`, `WorkingDirectory`, `User`, `Environment` and `ReadOnlyPaths` — the last with the exact value `/work`, the checkout being the only path that is mill's to protect — and refuses every other property by name; `stop` admits a `mill-` machine and a `mill-step-*.slice` or `mill-exec-*.service`; `terminate` admits a machine whose unit is a `mill-machine@` instance. Refusals exit 78. The fourth verb, `shutdown`, is the unit's own `ExecStop` and is not in the sudoers rule.
- The worker never runs `systemd-nspawn`, never holds root, and never reaches the helper except through the driver (`internal/workspace/machine/driver`), which is the one place `sudo` is spelled.
- **On `core` today** `/etc/sudoers.d/ivy` grants the operator `NOPASSWD: ALL` (homestead-apply relies on it), so for uid 1000 the helper is the interface the worker uses, not a boundary sudo enforces; the sudoers deliverable adds no reach there. The boundary holds for any other uid, and the role's shape was proved from a throwaway user with the identical rule.
- Every process inside the machine is unprivileged on the host through the user namespace, including root inside a `nested` machine.
- The image directory is root's (§Storage), so the image store's subvolume verbs run through `sudo -n` too (`Commands.Sudo`); lineages and run directories are the worker's and never do.
- Unprivileged nspawn through `systemd-nsresourced` (E7) would remove the template unit entirely, and does not work on `core`: SELinux denies `systemd_nsresourced_t` `read` on `nsfs_t`, so a `--private-users=managed` boot hangs in nspawn's varlink call rather than erroring, and nothing is registered. Lifting it is a homestead policy module (`allow systemd_nsresourced_t nsfs_t:file read`), not a mill change; every machine, nested or flat, is started by the root-owned template unit.
- The multi-tenant upgrade, `systemd-vmspawn` (E8), boots the same tree in about ten seconds from a `systemd-repart` raw image with the host's kernel and `rd.luks=0 rd.lvm=0` against the host's initrd; `-M` does not reach it, because vmspawn's transport is ssh over vsock and the image carries no `sshd`. The path there is `sshd` and vsock in the image and a virtiofs-capable initrd for `--directory`; it changes the boot command and nothing in the interface.

## SELinux

`core` enforces SELinux, and nothing in a machine is confined by it. `mill-machine@.service` execs `systemd-nspawn`, which is `bin_t`, from `init_t`, and the base policy's transition for that is `unconfined_service_t`: nspawn and every process in the machine — its systemd, `podman`, `dnf`, the compiler — run in that domain, and the spike (E1–E4, E13) recorded no denial with the image tree `unlabeled_t` and the workspace `var_lib_t`, nested or flat. So there is no `--selinux-context=`, no `--selinux-apifs-context=`, and homestead's `mill_selinux` role is file contexts only: `/var/lib/mill(/.*)?` is `mill_var_lib_t`. The boundary is the user namespace and the unit's cgroup. Confining the machine would be `--selinux-context=` naming a container domain and an image tree labelled `container_file_t` as mill builds it; the export function in `internal/workspace/machine/image` is the one place that label would go.

## External state

Processes are the easy half of cleanup: a slice reaps a step, a unit reaps a machine, an outer machine takes its inner machines with it. What leaks is everything a machine creates *outside* itself, and this repo has been bitten by that class three times in two days — an org-scope workflow row from one run failing the next (`mill#34`), sixty-nine `mill-acceptance-*` projects waiting for delayed deletion on the forge, scratch dagger projects that are archived only if a Ginkgo `DeferCleanup` happened to run.

The rule: **a machine may only create external state that is swept by name from the host side.** `DeferCleanup` is not a teardown mechanism when the process can be killed.

| External state | Named by | Swept by |
|---|---|---|
| Machines | `mill-<run-id>`, always: a suite's machines are its `mill run` subprocesses' and its worker's, and carry no suite name | worker reconciliation: any `mill-*` machine with no live run is stopped — as §Destroy stops one, or terminated when no record says it reported ready — and its run directory removed |
| Cache lineages | scope and name; nested runs get their own root | eviction, and the nested root dies with the outer's ephemeral root |
| Dagger scratch projects | `mill-acceptance-<scenario>-<nonce>` | worker reconciliation, through the worker's own client against the configured dagger: any matching project older than the suite's budget with no live run in it is deleted when the service offers a delete and archived when it does not, and the report says which. Dagger offers no delete today |
| Forge scratch projects | `mill-acceptance-<scenario>-<nonce>` in the scratch namespace (`scratch-namespace` in worker configuration) | worker reconciliation: any matching project older than the suite's budget is deleted through the forge adapter's namespace verbs |
| Workflow rows a suite derives | project scope, in the scenario's scratch project | swept with the project; never at org scope from a suite — `stageWorkflow` fails the scenario that asks for org scope |
| Run directories, `.building` images | run id, derived digest | worker reconciliation |
| Live runs whose node dagger has closed | the run's `node_ref`, asked of dagger by name | the worker's sweep (`docs/ledger.md` §Sweep): the run ends in the ledger and its workspace, branch and pull request go with it; never by readiness, only by dagger's closed status |

The pattern is `^mill-acceptance-<scenario>-<nonce>$` with an eight-hex-digit nonce, and the budget is the suite's own timeout, 90 minutes unless `scratch-budget` says otherwise: a project younger than that may belong to a suite still running. A live run is one the ledger holds open; a scratch project a live run's node is in is not swept whatever its age. The sweep matches by name and age and never by "everything not in the ledger" (mill#49).

Reconciliation runs the sweeps on the same pass it reconciles machines, and the pass is recorded in the ledger (`reconciliations`: the worker, when it started, and each finding — orphan, lost sandbox, resumable, leftover, swept, stranded, failure — with what was done). A suite that cannot name its state so that a sweep finds it has a bug in the suite, not a gap in the sweep.

## Reconciliation on resume

The ledger records per run: machine name, machine unit, derived digest, veth name, the lineages restored and, at the run's end, the generation each promoted (`docs/ledger.md` §workspaces). They travel on the workspace handle as `workspace.Sandbox`, filled by `Create` and `Attach`, and the run loop writes them when the workspace opens and when the run finishes. The run's own copy on the host is `runs/<id>/run.json`, which `Attach` reads before the ledger is asked anything and which also records the current step slice, the candidate snapshot's step, and when the machine reported ready. The ledger is the record; `run.json` is the host's copy. A pass that finds the two naming different machines for one run records a `disagreement` finding naming both and acts on the ledger's.

On worker start, before the first claim and in this order:

0. The sweep of the ledger against dagger (`docs/ledger.md` §Sweep): every live run of this worker's whose node dagger has closed is failed — or marked done, when its own completing patch had landed — its workspace destroyed through `Attach` and its branch closed. It goes first so the rest of the pass sees the ledger's truth: a run the sweep has just ended is no longer live, and the machine it could not reach is an orphan and its directory a leftover in the very next step. The same sweep runs again between claims, on its own interval.
1. `machinectl list` and compare with live runs:
   - a machine with no live run is an **orphan** → stop its unit (as §Destroy does: stop only if `run.json` says it was ready, else terminate), delete its run directory, record the fact.
   - a live run with no machine is a **lost sandbox** → the run fails with that as evidence — the machine's name and the ledger's step — when it is next claimed, or when the sweep finds its node closed: `Attach` refuses with it, the run is failed in the ledger, the node is held in dagger under the same reason so it is not claimed into the same refusal again, and the lease is released. Nothing resumes into a fresh machine; the workspace's state is not the ledger's to reconstruct. The pass records it and leaves the run directory, because the record is what tells the claim a machine once existed. A run that never had a machine on this host — one still selecting, or another host's — is not a lost sandbox.
   - a live run with its machine present resumes at the ledger's step after stopping and recreating the current slice. Whatever was running when the worker died is not trusted to still be the step: the pass stops the slice `run.json` names, `Attach` stops it again and recreates it under the same ordinal, so the run loop's count continues from where the dead worker's left off. The candidate snapshot from the last evidence-closed step is still valid and still promotes at finish.
2. Delete `*.building` under `/var/lib/machines/` — found through the build locks the worker's own image directory holds, since the machines directory is root's — and any run directory with no live run.
3. Sweep external state by name (§External state): stale `mill-acceptance-*` dagger and forge projects.
4. Record the pass in the ledger.
5. Run eviction.

No release path returns to `ClaimNext` for free. A refusal before any step runs either Holds the node under the reason that caused it — a lost sandbox (above), a project with no repository (`noRepository`), a done-or-failed run found on an open node, a workflow naming a harness this worker has no adapter for — so it is deterministic and every worker would refuse it the same way until a person or the workflow changes; or, for a refusal that is not deterministic enough to Hold (a forge outage, a dropped connection, anything else `openWorkspace`/`selectWorkflow`/`restoreRetried`/`resumePark` can return), the worker waits before reclaiming that node again, doubling the wait on each consecutive refusal up to a cap (`internal/run/worker.go`'s `BackoffBase`/`BackoffCap`), and logs once per refusal rather than once per cycle. The backoff is worker-local and in memory, cleared the moment a real takeover succeeds; Hold is dagger's and stands until someone acts on it. Together they are the general rule this document's specific cases above already followed one at a time: a claim on a node that cannot be opened right now must never come straight back to the same claim (mill#39).

A pass that cannot tell orphans from live runs — the ledger or machined not answering — stops the worker before it claims anything. Anything narrower is a `failure` finding and the rest of the pass stands.

Polling `machinectl` is sufficient at run cadence. If it is not, host-side D-Bus signals are the upgrade, for lifecycle only; exec stays on the CLI.

## Failure modes

| Failure | Detection | Response |
|---|---|---|
| Base digest unresolvable (Harbor down) | the pull fails at sequence step 4 | Use the last resolved digest (`/var/lib/mill/images/base.json`) if its subvolume exists, recording the fallback; else fail the run before boot |
| Derived build fails or times out | `podman build` exit / budget | Boot base; finding to first session |
| `mill.toml` invalid | parse/validate at step 3 | Defaults; finding to first session |
| Machine never reaches ready | readiness timeout | Terminate (never stop), remove the run directory, fail the run with the timeout and the digest |
| Helper refuses the readiness probe | exit 78 from the helper | Fail at once with the refusal: the worker's manifest or slice name is wrong, not the machine |
| Toolchain step fails | exit ≠ 0, or its budget spent | Continue; the step closes on the record, finding to first session; no retry consumed (§Preparatory steps) |
| Exec unit fails to start (not the command) | the helper's `systemd-run` non-zero before the service runs | Retry once; then fail the step with stderr as evidence. A helper refusal (78) is not retried |
| Command exits non-zero | `--wait` propagates | The step's contract decides |
| Step exceeds its budget | mill's timer | Stop the slice; no candidate snapshot; fail the step |
| Machine dies mid-step | exec returns a connection error; `machinectl show` reports none | Fail the run: lost sandbox. Candidate from an earlier closed step still promotes |
| Worker dies mid-step | §Reconciliation | Resume at the ledger's step with a fresh slice |
| Node dropped or completed underneath a run holding no lease | the worker's sweep asks dagger (`docs/ledger.md` §Sweep) | Fail the run with the reason — or mark it done when its own completing patch had landed — destroy the workspace through `Attach`, close the branch; no dagger write |
| Proxy unavailable | connections from the machine fail; the toolchain step's `mise install` is the first to hang | The toolchain step closes on its spent budget and the first session receives the finding; `mill doctor` does not probe the proxy, and squid's own log on the host is where the answer is |
| Cache filesystem full | promotion or restore fails with ENOSPC | Run eviction, retry once; then the run finishes without promoting |
| Lineage evicted mid-run | absent at promotion | Promote into a fresh lineage |
| Run directory removal fails | error on destroy | Finish; log; reconcile next start |

## Testing

The behaviour is specified as scenarios in [sandbox-scenarios.md](sandbox-scenarios.md). Each is a Ginkgo spec named for the scenario, in one of three tiers the tree already uses:

- **unit** — against the fake backend, no machine. `make test` runs it.
- **db** — `Label("db")`, a testsupport database. `make test` runs it.
- **live** — `Label("live")`, a real machine on a host with root, nspawn, btrfs and Harbor. The spec skips in `BeforeEach` when `MILL_SANDBOX_HOST` is unset, and the skip message names what running it costs, in the words the existing `live` specs use: *"set MILL_SANDBOX_HOST=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine."* `make test` runs it and it skips; on the host it runs. The acceptance suite's live specs sit behind its own gate too: under `make test` the whole of `tests/acceptance` skips at `BeforeSuite` for want of `MILL_ACCEPTANCE`, so their message shows only under `MILL_ACCEPTANCE=1` without `MILL_SANDBOX_HOST`.

There is no fourth tier and no new target. What makes the live tier real rather than a skip nobody triggers is §Self-hosting.

**Every suite runs against a throwaway dagger.** The acceptance suite files into the fixture in `internal/testsupport/dagger.go` by default (`mill#51`): a blank database from the shared testcontainers Postgres, dagger's own migrations applied with `goose` from a dagger checkout, one seeded org and one `user`-kind principal bound to the spec process's uid, one `dg serve` on a temporary socket, `SIGKILL`ed on cleanup. `make test-acceptance` with no socket variable creates nothing in the deployed dagger, the fencing and resume scenarios pass against the disposable instance, and a killed suite leaves nothing behind. Nothing the acceptance scenarios exercise — claim, heartbeat, fence, gate, complete, usage, holds — needs the deployed graph: fencing is per-lease and uses a second client under the same principal, and the seeded principal is `user`-kind, so it can answer a gate and place a hold.

The fixture is **per-spec** and stays that way: per-suite would share one graph across parallel scenarios, which is the cross-scenario pollution being removed. A spec that uses it carries the `db` label.

Two rules differ by tier:

- The **local-workspace suite** keeps `MILL_ACCEPTANCE_DAGGER_SOCKET` as a deliberate opt-in to the deployed dagger, for a `#38`-style proof on the deployment and for checking whether the real graph agrees with the throwaway one. The runbook says which is which.
- The **live tier refuses** any socket the environment names as the deployed dagger, flag or not. It starts machines through a root-owned unit and `mill run` subprocesses inside them, and `mill#49` already showed a test process becoming a worker against the live graph with no session involved. A flag a stray variable can flip is not a safe default for that tier.

**In a machine, the fixture's prerequisites are mount contents**: dagger's store migrations from its checkout, `goose` and the `dg` binary. A `nested` machine gets the host's dagger checkout bind-mounted read-only at `/var/lib/mill/dagger` — worker configuration's `dagger_checkout`, granted with `nested` and never otherwise (§Provision) — and the suite inside finds it through `MILL_DAGGER_REPO`. That checkout tracks dagger's `main`: nothing pins it, because mill's `go.mod` has no dagger dependency and dagger has no tags (`docs/runbook.md` §"The live tier"; a pin is a roadmap item). `dagger#25`, a published dagger image with an exported fixture, is what replaces the bind with a pull.

If mill ever takes an `agent`-kind principal of its own — so dagger can enforce the gate rule against it — the fixture needs **two** seeded principals, the mill and the person, because today a gate is answerable only because both are the same one. `seedDagger` is a four-row insert; the change is trivial, the fact is not.

The live suite boots no machine of its own: every machine is `mill-<run-id>`, made by the `mill run` or `mill work` it drives (§Naming), and is swept as an orphan once no live run holds it. What carries the `mill-acceptance-<scenario>-<nonce>` name is the scratch project on the forge and in dagger, and that name is what §External state's sweep matches after a kill.

## Self-hosting

Mill builds mill, and the sandbox is the thing being built, so the sequence matters.

**The privilege wall.** Sessions in the build loop run as the mill account with no root. `systemd-nspawn` needs root. So an agent working a sandbox node on the local workspace cannot execute the live tier, and the reviewer — "a criterion you cannot verify is a fail" — cannot verify it either. Root inside a machine is a different matter (§Provision): the namespace is the boundary, and giving `agent` sudo there costs nothing the design has not already accepted.

**The sequence.**

1. **Build on local, gate once.** Agents implement the backend on the local workspace. The live specs exist, are labelled, skip with the right message, and assert the scenarios' Then clauses — that is what the reviewer checks, and a spec named for a scenario that asserts nothing is a fail. The milestone that creates the backend ends at exactly one `assignee_kind: user` node, shaped like `mill#38` and filed as `mill#71`: run `MILL_SANDBOX_HOST=1 make test` on `core`, complete the gate with the ginkgo summary as its result. The loop drains to `WAITING: nothing claimable` when only that node remains, which is a clean stop. Its acceptance criterion is the output, not a person's say-so, so that a skipped tier cannot be mistaken for a passing one during the window the backend is least trusted. `mill#69` ran the whole of `tests/acceptance/sandbox_test.go` on `core` first — six specs, every one passed — so what the gate confirms is a run a person watched, not a first attempt.
2. **The selector.** `workspace.Kind` names `KindLocal`, `KindMachine` and `KindRemote`, and `run.Workspaces` — a function type — is chosen by it: `workspacesFor(...)` in `internal/cli/run.go` hands the loop `localWorkspaces(...)` or `machineWorkspaces(...)` according to `--workspace` on `serve`, `work` and `run`, or `workspace:` in the configuration file, with `local` the default; `remote` is refused as not built, and a name outside the three is refused by name. The backend behind `machineWorkspaces` is `internal/workspace/machine` over its `Driver` seam, and `checkWorkspace` runs before any claim, so a host that cannot run the configured kind — no btrfs under the sandbox root, no `/run/mill` — refuses at start. Moving a deployment to machines is that one line of configuration, which homestead's `mill` role writes (`workspace: machine`). The kind is `machine`, not a rootless container, which [ADR-018](adrs/018-a-runs-sandbox-is-a-systemd-nspawn-machine.md) rejects.
3. **Move the mill repo into machines with `nested`.** Worker configuration grants `[machine].nested` for the mill repo (`nested_repos`, §"Nested, exactly"; homestead's role lists `wellspring/mill`), and the repository's `mill.toml` asks for it and declares `btrfs-progs`. From here the live tier runs inside the agent's own machine as a check step under the workflow's `timeout`, on every node of mill's own, with an exit code as evidence: the mill project's workflows — `workflows/mill-quick-fix` and `workflows/mill-feature`, project-scoped definitions that shadow the org ones for `mill` alone and are otherwise identical — carry a `live-tier` check after `clean-tree` whose command is the tier's own invocation, `MILL_SANDBOX_HOST=1 mise exec -- make test`, on the tree the session committed. `mill#89` is the first run so delivered; `mill#72`'s result is its ledger. No skip, no gate, no `WAITING`.

   **What the tier is inside a machine.** The specs that need the host say so and skip — `testsupport.InsideMachine()` reads `/run/systemd/container` — naming what they need: the template unit and helper that boot a machine (`internal/workspace/machine`'s live and nested suites), the host's podman and image volume (`image`'s), the subscription credential (`gateway`'s), a forge and `MILL_ACCEPTANCE` (`tests/acceptance`). The specs written for the inside run: the cache lineages on the machine's ephemeral root (`internal/workspace/machine/cache`, `mill cache prune` in `internal/cli`), under the nested worker's root `/home/agent/mill`, which is why the repository declares `btrfs-progs`. The unit and db tiers run entire, the db tier against the machine's own podman with the fixture's reaper off — a privileged container does not start inside a machine, and the ephemeral root is the backstop the reaper would have been. So the check's exit 0 says: every spec of mill's that can run inside a machine passed inside one, and every spec that cannot said what it needs. Two things widen it, each a node of its own: a spec that drives §"Nested, exactly"'s inner boot from inside a nested machine, and a `dg` binary the dagger fixture can start from the checkout bound at `/var/lib/mill/dagger`, so the run-loop specs run inside too.
4. **Other repos follow without `nested`.** They never boot machines; they get the default provision.

**Why the gate is once.** You cannot test the first machine from inside a machine. After the first, the sandbox tests the sandbox. One `#38`-style node, ever, is the price of the bootstrap; a gate per milestone was the price of not being willing to give root inside, and that price was never worth paying.

### Nested, exactly

Worker configuration grants it; the repo only asks. The keys, in the worker's `config.yaml`, spelled with underscores the way a deployment spells `env_mode`:

```yaml
nested_repos:                    # repositories granted [machine].nested, by forge path or origin URL
  - wellspring/mill
dagger_checkout: /var/lib/mill/dagger   # the checkout bound read-only into a nested machine; this is the default
```

A repo is granted when `nested_repos` names its path on the forge (`wellspring/mill`, matched against the origin's path however the origin is spelled, case aside) or its origin URL. A request from any other repo is the finding *mill.toml: machine.nested: requested but not granted by worker configuration (nested_repos does not list this repository); the machine is provisioned without nested*, delivered to the first session the way every provisioning finding is (§Preparatory steps), and the provision is the flat one: no sudo, no binds, the flat image.

**What the outer gets** is §Provision's three changes and nothing else: the `nested` image variant, the two read-only binds, the helper's `/.hostsys`. **What the inner needs** is what the spike's E13 found and this repo's live tier does, as `agent` in the outer through its `sudo`:

1. **Snapshot the image into the outer's own tree first.** `--ephemeral` snapshots beside its source, and the bind at `/var/lib/machines` is read-only, so `btrfs subvolume snapshot /var/lib/machines/mill-image-<digest> /var/lib/mill-machines/mill-image-<digest>` — a reflink on the same filesystem — and the inner boots `--ephemeral` from that. The bind stays read-only.
2. **A fresh tmpfs on `/run/systemd/nspawn`.** Every mount the outer inherited from its boot carries the kernel's locked `nosuid,nodev` flags, and nspawn's mount tunnel remounts a bind of `/run/systemd/nspawn/propagate/<machine>` read-only with a bare `MS_RDONLY`, which the kernel refuses on a locked mount (`Failed to mount n/a on …/run/host/incoming: Operation not permitted`). A tmpfs mounted inside the outer is not locked: `mkdir -p /run/systemd/nspawn && mount -t tmpfs tmpfs /run/systemd/nspawn` before the first inner boot. (The spike's transcript did not need this on the same host and kernel; nothing in it explains why, and the refusal reproduces on every image and boot form since.)
3. **The inner's argv**: `systemd-nspawn --boot --quiet --machine=inner --directory=/var/lib/mill-machines/mill-image-<digest> --ephemeral --private-users=identity --network-veth --settings=false --bind-ro=/proc:/.hostproc --bind-ro=/.hostsys:/.hostsys`, under `systemd-run --unit=inner --collect` so the outer's manager owns it. `identity` because it is the only mode that works one level down; the two binds because the inner's nspawn needs a fully visible `proc` and `sysfs` in its tree when it pivots, and the outer's `/proc` (unmasked by `mill-proc.service`) and the helper's `/.hostsys` are those.

Then `machinectl list` in the outer names `inner`, `systemd-run -M inner --pipe --wait -- <argv>` runs in it with the command's own exit status, and `podman` inside the inner pulls through Harbor as it does one level up. **Inner egress** is through the outer: the image's `systemd-networkd` applies systemd's stock `80-container-ve.network` to `ve-inner`, whose `IPMasquerade=both` turns on forwarding and masquerades the inner's `/28` behind the outer's address, so the host's squid and its `inet mill` table see only the outer. No second squid, no rule in the outer, nothing on the host; an inner suite's network scenarios run against exactly what the outer provides, which is the host's proxy at `10.99.255.1:3128` and nothing direct. Stopping the outer's unit ends the inner with it: the inner is a service of the outer's manager, in the outer's cgroup tree, and `machinectl list` on the host never knew it.

**The inner worker's root.** A worker that finds itself inside a machine — `systemd-detect-virt --container` says `systemd-nspawn`; `mill run`, `mill work`, `mill cache ls` and `mill doctor` all ask — defaults its sandbox root to `/home/agent/mill`, a subvolume it creates under the machine's own ephemeral root as `agent`, with no privilege, when neither `--sandbox-root` nor `sandbox-root` in the file names one. `Check` refuses a root that is not on the ephemeral root — one on a bind, such as `/var/lib/machines` or `/work` — before a claim, and the cache store refuses a promotion addressed outside its root (§Caching). So an inner run's lineages live under `/home/agent/mill/cache`, its generations are discarded with the outer, and the outer's lineages on the host gain nothing from it.

**Nested and unprivileged are two different boots.** `nested` wants sudo inside and the helper's `/.hostsys`; an unprivileged outer (§Privilege, E7) wants no template unit outside, and on `core` it does not boot at all, for the policy reason §Privilege states. So every machine, nested or flat, is started by the root-owned template unit under polkit, and whether an unprivileged outer could host an inner is a question the policy module would have to be written before anyone could ask.

## What the host provides

The sandbox has prerequisites mill cannot provide for itself. They are homestead's, one role each, and `core` carries them:

- **`mill_storage`** — the btrfs volume (§Storage): `nvme1n1` under LUKS, mounted at `/var/lib/mill` with `user_subvol_rm_allowed`, its `machines` subvolume at `/var/lib/machines`.
- **`mill_machine`** — `systemd-container`; the template `mill-machine@.service`, its helper `/usr/libexec/mill-machine`, the policy `/etc/mill-machine.json` (the read-only sources the helper admits: the worker's share root, and under `nested` the image subvolumes and the dagger checkout), the polkit rule `50-mill-machine.rules`, and the sudoers rule for the helper's three verbs (§Privilege).
- **`mill_egress`** — the `mill-host` dummy link at `10.99.255.1`, the `inet mill` nftables table and its loader unit (its own table, never a flush, in `thicket_nftables`'s style), the DHCP pool on `ve-mill*`, and squid on `:3128` with the pool admitted and the LAN denied (§Network).
- **`mill_selinux`** — the file context for `/var/lib/mill` and the spec that a machine runs with no denial (§SELinux).
- **`mill`** — `contrib/systemd/mill.service` as the operator's user unit with `workspace: machine` and the rest of the sandbox keys in `~/.config/mill/config.yaml` (every key in its long-flag spelling, and `nested_repos`, `dagger_checkout`, `model_credential_minted` with underscores — the hyphenated spelling sets nothing, `docs/tools.md` §Configuration); `LoadCredential=claude-oauth` from `vault_claude_code_oauth_token` and `LoadCredential=harbor-robot` — one line, `robot:secret` — for the image timer; `/etc/tmpfiles.d/mill.conf` making `/run/mill` the operator's; the weekly image timer; and `/var/lib/mill/dagger`, a clone of dagger's `main` owned by the operator.
- **`mill_forge`** — the acceptance suite's forge credentials, `~/.config/mill/acceptance.env`.

Homestead's own ADR names the machine as the third runtime class on the host — neither a Quadlet service nor a libvirt guest — and reconciles mill running under the operator's account with a session that is not that account: the user namespace is the boundary. What is left outside both repositories is dagger's: `dagger#25`, a published image with an exported fixture, so the machine's dagger is a pull rather than a bind of a checkout, and a real delete for scratch projects — hygiene for the deployed path only, since the throwaway instance's cleanup is `DROP DATABASE`.

## Findings against the tree

What is wrong or missing today, each a node or a roadmap item of its own:

- Nothing pins the dagger checkout a nested machine is given: `go.mod` has no dagger dependency and dagger has no tags, so `/var/lib/mill/dagger` tracks `main` and a dagger change can fail the fixture inside a machine before mill changes at all (§Testing; `docs/roadmap.md`).
- `mill doctor` probes none of the template unit, the helper, `/run/mill`, the proxy, Harbor or the base image subvolume; the first run that needs each is where its absence shows (`docs/tools.md` §doctor).
- A container started through the machine's podman API socket is not under the step's slice and outlives the step until machine teardown (§"Step boundary").
- The machine's podman API service is socket-activated with podman's five-second idle exit and systemd's default `TriggerLimitBurst`, and a container it started sits in its own cgroup: `mill#89`'s tier inside a machine saw the service refuse to restart with `Failed to spawn executor: Device or resource busy` while containers from a failed start remained, and the socket unit hit its trigger limit under fifteen ginkgo processes connecting at once. The db tier passes with the reaper off inside a machine (`internal/testsupport`); an image that runs the service with `--time=0` and a raised burst is the fix, and a rebuild of the base (`images/base`).
- No `dg` binary is inside a machine, so the dagger fixture's specs skip there even in a nested machine with the checkout bound at `/var/lib/mill/dagger`; and no spec drives the inner boot §"Nested, exactly" describes from inside a nested machine (§Self-hosting, step 3).

## Settled by the spike

- `--private-users-ownership=auto` idmaps the root tree only; the workspace bind is the same uid on both sides through the bind option `idmap`, under SELinux enforcing, with no denial (E4).
- A `podman run` lands under the step slice only with `--cgroups=split`; without it, under `user@1000.service`. The base image pins it (E3).
- testcontainers' reaper runs against `podman.socket` inside the machine and removes what a suite started once the suite's connection drops; the reaper's own container is under `agent`'s user manager, not the step's slice (E6, `mill#69`).
- The derived-image export stays `podman build` plus a flat export (E10: build 4.3 s, export 0.8 s, a derived subvolume the size of base plus the package). Exporting onto a snapshot of base is the change to make if `mill image ls` ever shows dozens of derived images.

## Future considerations

Not supported, not designed for, recorded so the design does not accidentally foreclose them.

### A workflow that boots a VM inside the machine

The case: an agent selects, builds and tests kernel patches with `kpatch` — build the module against a specific kernel, boot that kernel in a guest, load the module there, run tests, push. It fits the design with one addition the spec currently forbids, and it is worth keeping in view because it stresses the parts that are still abstract.

- **The build is ordinary.** `kpatch-build`, `qemu-kvm` and `virtme-ng` are `[image].packages`; versioned kernel packages change the derived digest as they should. `kernel-debuginfo` stays out of the image and in `kpatch-build`'s own cache dir, which becomes a repo-scoped lineage alongside `~/.cache/ccache` — skipping a kernel rebuild would be the largest win the cache design ever delivers. Step budgets in hours and `MemoryMax` in the tens of GB are workflow-declared already; eviction headroom would need to cover a kernel build's ephemeral root.
- **The guest is the one addition.** `/dev/kvm` is not mounted today (§Filesystem). Supporting this means a declared, granted capability — `[machine].kvm = true` requested by the repo, allowed by worker config, unmet becoming a finding — realised as `DeviceAllow=/dev/kvm rw` on the machine unit plus a bind. The node is 0666 on `core` (§The host), so uid 1000 inside the user namespace can open it. Nothing else escalates: the module loads inside the guest, so the machine never needs `CAP_SYS_MODULE`; `virtme-ng` uses user-mode networking, so no `/dev/net/tun`. The boundary widens by one device node.
- **Evidence is an exit code.** `virtme-ng` boots a kernel in about a second with the machine's root as the guest's userspace, runs a command, and returns its status. The check step is `vng --run <vmlinuz> -- 'kpatch load patch.ko && ./run-tests'`; principle 8 is satisfied without parsing anything.
- **It is an argument for nspawn over vmspawn.** In a system container the guest uses the host's KVM directly. Under `systemd-vmspawn` this becomes nested virtualisation — workable on this hardware, slower and less reliable. When multi-tenancy pushes toward vmspawn, this is the workflow that pays.
- **It makes a `dnf` cache on the worker the first step, not the fallback.** Kernel debuginfo through the proxy to Fedora mirrors on every lineage reset is many GB per fetch.

The authority split holds without a second rule: the repo declares packages, caches and a device request; the worker decides whether to grant it and what limits apply.
