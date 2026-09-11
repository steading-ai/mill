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
- Host configuration mill cannot ship — the storage volume, the machine template unit and its polkit rule, the nftables table, squid, the SELinux module, the credential delivered by `LoadCredential` — is homestead's, the operator's host-configuration project, and reaches `core` as roles there (§Sequencing across repositories).

**The spike.** The experiments this design depends on — numbered E0 to E14 below — are run on `core` by a person, outside this repository, before the backend milestone starts; a section that names one records its result here when it is known, and until then the section states the design that the experiment decides.

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

- Machine: `mill-<run-id>`. The reconciliation key; **MUST** be derivable from the run alone.
- Machine unit: `mill-machine@<run-id>.service` on the host, an instance of the root-owned template (§Privilege).
- Step slice: `mill-step-<ordinal>.slice` inside the machine.
- Exec unit: `mill-exec-<uuid>.service` inside the machine, under its step's slice.
- Image subvolume: `/var/lib/machines/mill-image-<derived-digest>`.
- Lineage: `/var/lib/mill/cache/<scope>/<name>/` where scope is `repo/<repo-id>` or `global`; generations are `gen-<n>` inside it; `manifest.json` beside them.
- Run directory: `/var/lib/mill/runs/<run-id>/` containing `workspace/` and `cache/<name>/` (the run's writable snapshots).

## Sequence of a run

Everything below is in this order. The order is the design: configuration is read before the thing it configures exists, and nothing persists except through promotion.

1. **Claim.** The run exists in the ledger; the run directory is created.
2. **Clone.** The worker clones the node's repository into the workspace dir on the host, with the worker's own forge credential. No session credential is involved.
3. **Read `mill.toml`** from the checkout. Absent is valid and means defaults. Invalid is a finding (§mill.toml), not a failure.
4. **Resolve the base** digest from Harbor (`mill/base:latest` unless worker config pins a tag or digest). Compute the derived digest.
5. **Ensure the image subvolume** (§Derived images). On build failure, fall back to base and record a finding.
6. **Restore caches** (§Caching): a writable snapshot per lineage into the run directory.
7. **Boot the machine** (§Provision) with the workspace, socket, skill and cache binds.
8. **Wait for ready.**
9. **Toolchain step** (§Preparatory steps): `mise trust /work && mise install`.
10. **Workflow steps**, each under its own slice; caches snapshotted at each evidence-closed step.
11. **Finish**: promote the caches from the last evidence-closed step; record the result; destroy the machine; remove the run directory.

## Images

### Base

- Built from `images/base/Containerfile` by `mill image build-base --push`, run weekly by `contrib/systemd/mill-image-build.timer` on `core` (a user unit beside `contrib/systemd/mill.service`) and on demand; pushed to Harbor as `mill/base:<date>` and `mill/base:latest` with a robot credential delivered by `LoadCredential` as `$CREDENTIALS_DIRECTORY/harbor-robot`. `images/base/README.md` lists what is in it and what the export step must do to the tree. **Not by forge CI**: CI is not active on the GitLab projects, and the host that runs machines is the host that has podman and the credential.
- **MUST** contain: systemd as PID 1, `dbus-broker`, `podman` with `podman.socket` enabled and `storage.conf` selecting `overlay`, `git`, `sudo`, `mise`, and the native-extension toolchain — gcc, g++, make, pkgconf, python3, and the `-devel` set ruby-build, node-gyp and linked crates require (openssl, zlib, libffi, libyaml, readline, ncurses, gdbm, sqlite). Node installed globally through mise for the harness's runtime.
- **MUST** contain the user `agent` (uid 1000, home `/home/agent`), the `dnf` shim (§The dnf shim) and its sudoers rule, `registries.conf` routing pulls through Harbor's proxy projects, and a pre-granted `mise trust` for `/work`.
- **MUST NOT** contain credentials, the mill skill, the sidecar, or any of the operator's `$HOME`. The skill and sidecar are the worker's and are mounted at provision.
- The worker resolves the base digest at each provision. A new base takes effect on the next run; rollback is a pin in worker config (`base-image`, a tag or a digest).
- What the spike found the exported tree needs, and where each lands: the file capabilities on `newuidmap` and `newgidmap` are restored by the export step, because `podman export` drops them and the Containerfile cannot put them back; `agent`'s subordinate ranges (`10000:55536`, inside the machine's 65536 uids), the `mill-proc.service` unit that mounts a pristine `proc` before `agent`'s user manager, and `containers.conf` with `cgroups = "split"` are baked into the image.

### Derived images

- The derived digest is `sha256(base digest ‖ sorted, deduplicated package list)`.
- The build is on demand (`internal/workspace/machine/image`, `Ensure`):
  1. Take `flock` on `/var/lib/mill/images/<digest>.lock`. A concurrent run of the same repo waits here. The lock is held on a descriptor podman never inherits: `flock <file> cmd` leaks it into `pasta`, which logs a denial per build.
  2. Under the lock, if `mill-image-<digest>` exists, use it.
  3. Otherwise generate `FROM <harbor>/mill/base@<digest>` + `RUN dnf install -y <packages>`, `podman build` it on the host (through the workspace chokepoint; this is the one place the worker runs a container outside a machine, and the code **MUST** make that visible), export the container's filesystem into `mill-image-<digest>.building` as a subvolume — `podman create`, `podman export | tar -x`, a flat tree and not layers — restore the two file capabilities, then rename atomically to `mill-image-<digest>`. The export is one function so that a label for a confined machine (§SELinux) has one place to go.
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

The worker writes `/var/lib/mill/runs/<run-id>/machine.json` — the image subvolume, binds, limits and whether `nested` is granted — and starts `mill-machine@<run-id>.service` (§Privilege). The template's `ExecStart` reads that file and becomes:

```
systemd-nspawn --boot --quiet \
    --machine=mill-<run-id> \
    --directory=/var/lib/machines/mill-image-<derived-digest> --ephemeral \
    --private-users=pick --private-users-ownership=auto \
    --network-veth \
    --bind=/var/lib/mill/runs/<run-id>/workspace:/work \
    --bind=/run/mill/<run-id>:/run/mill \
    --bind-ro=<worker skill dir>:/home/agent/.claude/skills/mill \
    --bind=/var/lib/mill/runs/<run-id>/cache/<name>:<path>   # one per cache
    --settings=false
```

with `MemoryMax=`, `CPUQuota=`, `TasksMax=` and `KillMode=mixed` on the unit (§Resource limits).

- `--ephemeral` **MUST** be used: the root is a btrfs snapshot of the image subvolume, discarded on exit.
- `--private-users=pick` allocates a 64K uid range per machine; two machines never share one.
- `--settings=false` stops `.nspawn` files on the host from altering the invocation; everything the machine is, is on the command line and therefore in the ledger.
- When `[machine].nested` is granted, three things change and nothing else: `--private-users=<base>:1048576` instead of `pick`, so an inner `--private-users=pick` has a range to choose from; `agent` has passwordless `sudo` for everything, because root inside the user namespace is an unprivileged uid on the host and the namespace, not the uid, is the boundary; and the host's image subvolumes are bind-mounted read-only at `/var/lib/machines` so the inner suite boots them rather than building them. The host's btrfs **MUST** be mounted with `user_subvol_rm_allowed`, or inner teardown and eviction cannot delete subvolumes.
- Readiness: poll `machinectl show mill-<run-id> -p State` for `running`, then exec `systemctl is-system-running --wait`. Readiness **MUST** have a timeout; a machine that does not reach ready is destroyed and the run fails with that evidence.

### Exec

Every process a step runs, including checks mill runs itself:

```
systemd-run --machine=mill-<run-id> --pipe --wait --quiet --collect \
  --unit=mill-exec-<uuid> \
  -p Slice=mill-step-<n>.slice \
  -p WorkingDirectory=/work \
  -p User=agent \
  -p Environment=<k=v> ... \
  -- <argv>
```

- `--pipe` connects the caller's stdin, stdout and stderr to the service. Mill **MUST** stream them.
- `--wait` propagates the main process's exit status. That status is what `Workspace.Exec` returns and what evidence checks read.
- `--quiet` keeps `systemd-run`'s own messages off the step's stderr. `--collect` unloads the unit when it ends.
- `Slice=` is **REQUIRED**. An exec with no step slice is a bug.
- Cancellation is `systemctl --machine=mill-<run-id> stop mill-exec-<uuid>.service`, or the slice. Killing the `systemd-run` client does not stop the unit.
- The exec's `Environment=` is its only environment. There is no machine-wide environment for steps.

### Step boundary

- A step **MUST** end with `systemctl --machine=mill-<run-id> stop mill-step-<n>.slice`. This kills everything the step started, daemons and containers included.
- The next step's slice **MUST NOT** be created until the previous one reports inactive.
- When a step closes on evidence, mill **MUST** take a read-only snapshot of each of the run's cache copies before stopping the slice. That snapshot is the promotion candidate; a later evidence-closed step replaces it. A step that is cancelled, times out, or dies leaves no candidate.
- Podman containers inside the machine run under the step's slice (podman's systemd cgroup manager; `--cgroups=split` if the spike shows it necessary). A workflow that wants a container to outlive a step has misdesigned the step.

### Destroy

- `systemctl stop mill-machine@<run-id>.service`. `KillMode=mixed` lets nspawn shut the machine down, then kills the rest.
- The ephemeral root is discarded by nspawn. The run directory — workspace, socket dir, cache copies — is the host's, and mill **MUST** remove it after promotion and after any evidence it needs from the workspace is recorded. Removing a cache copy is `btrfs subvolume delete`.
- A run **MUST NOT** be marked finished while its machine is still registered.

## Filesystem

| Path in machine | Backing | Mode | Lifetime |
|---|---|---|---|
| `/` | ephemeral snapshot of the image subvolume | rw | machine |
| `/work` | `runs/<id>/workspace` on host | rw | run; mill deletes it |
| `/run/mill` | `/run/mill/<id>` on host | socket only | run |
| `/home/agent/.claude/skills/mill` | worker's skill dir | ro | worker version |
| cache paths | `runs/<id>/cache/<name>`, a writable snapshot of a generation | rw | run; promoted or deleted |
| `/var/lib/containers` | ephemeral root | rw | machine |

- The workspace is bind-mounted rather than cloned inside so the host can read evidence from it and keep it for a post-mortem when a run fails. `--private-users-ownership=auto` idmaps the bind so uid 1000 is uid 1000 on both sides.
- Container storage is ephemeral. Podman's storage carries a database and locks and is not safely snapshottable under a running daemon; image pulls are made cheap by Harbor's proxy cache instead (§Network).
- No other host path is mounted. Not `/dev/kvm`, not the host's podman socket, not the operator's `$HOME`, not the mill binary.

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

## Eviction

Three mechanisms, applied in this order at each trigger.

1. **Within a lineage**: keep the two newest generations. Applied at promotion.
2. **Expiry**: a repo-scoped lineage or an image subvolume not restored or booted for 30 days (worker config) is deleted.
3. **Watermarks**: when free space on the cache filesystem — `statfs`, the truth — is below the high watermark, delete repo-scoped lineages and image subvolumes in order of least recent use until free space exceeds the low watermark. Two thresholds so eviction does not run at every promotion. Global lineages are not evicted by watermark; they are bounded by their caps (§Promote).

- After each `btrfs subvolume delete`, `btrfs subvolume sync` before re-reading free space; deletion frees space asynchronously and an eviction loop that does not wait over-evicts.
- Do not enable qgroups for this. Per-lineage sizes are estimates for ranking victims; free space decides whether to evict.
- A lineage under eviction may be the parent of an in-flight run's writable snapshot. That is safe: btrfs snapshots are independent. A run whose lineage was evicted mid-run promotes into a fresh lineage.
- Triggers: after each promotion; at worker start, with reconciliation; on a systemd timer for expiry.
- A per-lineage cap on repo scope is a number `mill cache ls` flags, not a gate on promotion. A project whose build state is naturally large is still cached; the watermark decides.
- Operator tools: `mill cache ls`, `mill cache prune`, `mill image ls`, `mill image prune`. The image pair is in `docs/tools.md`; `prune` is expiry alone until the eviction node adds watermarks.
- Later, not now: consult dagger's ready set so repos with pending work are protected from eviction. LRU first.

## Network

- The machine has a private network namespace on a host-side veth named `ve-mill-<run-id>` (truncated to the interface-name limit; mill **MUST** record the actual name). The host addresses it the way systemd's own `80-container-ve.network` does — a DHCP server on the host side of every `ve-mill*` link, no masquerade — with NetworkManager told to leave `ve-mill*` unmanaged.
- The host's nftables table is **static**, because the worker is unprivileged and cannot add rules at run time: one `table inet mill` matching `iifname "ve-mill*"`, permitting DNS to the host resolver, TCP to the proxy and gateway ports on the host, and established/related; dropping everything else, including forwarding between `ve-mill*` links and to the LAN. Its own table and its own loader unit, never a `flush ruleset`, because firewalld owns `inet firewalld` on `core` (the pattern homestead's `thicket_nftables` role already uses).
- Egress is a forward proxy on the host — squid, or something equally plain — listening on the host side of each machine's veth. It is **transparent in policy**: no allowlist, no interception, `CONNECT` passed through so the TLS a session sees is the origin's. Its job for now is to be the only path out, so the network architecture is settled and a smarter proxy can replace it without the machines noticing. `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` are set in every exec's environment.
- The nftables drop is what makes the proxy mandatory, not the proxy itself. A session that ignores the environment and connects directly gets a timeout, not a bypass.
- **The proxy variables MUST cross both environment floors.** The harness adapter (`envFloor`, `internal/harness/claude/environment.go`) and the sidecar (`ENVIRONMENT_PASSTHROUGH`, `sidecar/src/session.ts`) are allowlists of eleven names, and the SDK does not merge an exec's environment with them. Setting the variables on the exec alone leaves a session's shell with no proxy on a host that drops the direct path: every outbound call hangs to its timeout with nothing naming a proxy. They are copied past both floors the way `mill#50` copies the workspace-git variables — as decisions the workspace made, not values the host supplied — in both files, which already point at each other.
- The session **MUST** be told. The mill skill and the session's system prompt (set by the harness adapter) state that all egress goes through the proxy named in the environment, so an agent debugging a failed connection reaches for the right explanation.
- Per-workflow allowlists are a later addition to the proxy's configuration, not to the machine's. When they arrive, the workflow's declaration — the model API, the forge, Harbor, distro mirrors — becomes the proxy's rule set, and nothing else in this section changes.
- **Container images come through Harbor's proxy-cache projects.** The base image's `registries.conf` rewrites `docker.io`, `quay.io`, `ghcr.io` to the corresponding Harbor project. Nothing pulls from an upstream registry directly, and the machine's container store can stay ephemeral because a second pull of the same image is a LAN fetch.
- RPM repositories are not proxied by Harbor; `dnf` reaches the mirrors through the proxy. Fedora's mirrors are HTTPS, so squid's own cache does not help without interception; a `dnf` cache on the worker is the fix if it proves noisy.
- Containers the test suite starts inside the machine publish ports on the machine's loopback, which is what testcontainers expects. They are not reachable from the host.

## Credentials

- The store issues a token per step. It **MUST NOT** enter the machine; the host side of the workspace socket holds it.
- A session needing a derived credential — git, forge, registry — asks over the socket; the host mints it against the step's token. Derived credentials **MUST** be short-lived and **MUST** be revoked when the step slice stops.
- Git credentials are delivered by a credential helper in the base image that talks to the socket; no token is written under `/work` or `/home/agent`.
- **The model credential never enters the machine.** A session on the account's login reads `~/.claude/.credentials.json`, which is the account's OAuth *refresh* token — the one credential on the box that outlives every step and that a session's shell can read. In a machine there is no such file. Instead the session holds the step token and the host holds the subscription, with the **credential gateway** (`internal/gateway`) between them:
  - every exec sets `ANTHROPIC_BASE_URL` to the gateway on the host side of the veth, beside squid, and `ANTHROPIC_AUTH_TOKEN` to the **step token** — copied past both environment floors as a workspace decision, like the proxy variables (`workspace.Egress`). Claude Code sends `ANTHROPIC_AUTH_TOKEN` as `Authorization: Bearer` above every file-based credential, so a session with no login and that variable set authenticates through the gateway alone. `ENABLE_TOOL_SEARCH=true` goes with them, because the CLI turns its MCP tool search off when the base URL is not a first-party host and the gateway forwards bodies unchanged (spike §E14);
  - the gateway listens where configuration says — `gateway-listen`, default `10.99.255.1:8080`, the `mill-host` dummy link homestead's `mill_egress` role creates and the one port besides squid's that the `mill` firewalld zone and the `inet mill` table admit from `ve-mill*` — and is turned on by `gateway: true`; off, a run behaves as it always has, on the account's login, which is what a laptop `mill run` gets. The local backend passes the same two variables when it is on;
  - it forwards the Messages API — `POST /v1/messages` and `/v1/messages/count_tokens` — and answers 404 to everything else. It checks the bearer against the token the run loop last rotated in for a live attempt, by hash, exactly as the workspace socket does, and refuses anything else with 401: `Rotate` at the step boundary is what retires the previous step's token. It swaps the bearer for the subscription credential, rewrites `Host`, drops the hop-by-hop headers, and forwards the rest of the CLI's headers — `anthropic-beta`, `user-agent`, `anthropic-version` — unchanged, which is all the upstream wanted (spike §E14). Streamed responses are flushed as they arrive; a client that abandons a stream, as the CLI's parallel title request does, ends that response and nothing else;
  - the upstream credential is a `claude setup-token` — subscription-billed, one year, interactive once — read from `$CREDENTIALS_DIRECTORY/claude-oauth`, delivered to the worker by `LoadCredential` from `vault_claude_code_oauth_token`, or from the file `model-credential-file` names. Never from the environment, and never logged: the log names a step token by its id and the credential by a fingerprint of its hash;
  - the gateway records every response's `usage` block as a ledger row `(step run, model, input, output, cache read, cache write)` with source `gateway`, distinct from the adapter's row with source `sdk` for the same turn. The adapter's rows are the step's cost — what `mill status` shows and what is posted to dagger — and the gateway's sit beside them: `mill inspect` carries them as the attempt's `gateway_totals`, and `mill cost` lists both, one row per source;
  - a step whose workflow declares `token_budget` — every token the attempt bills through the gateway, input, output, cache read and cache write summed — is refused with 429 and a body naming the budget once the attempt has spent it; the run loop reads the refusal when the session ends and fails the attempt on it, whatever the session made of being refused, so the budget is the failure the ledger, the retry prompt and the park gate carry. A step whose workflow declares `models` is refused 400 for a request naming a model outside the pin; an entry matches a model id it equals or that contains it, so `sonnet` pins every Sonnet — and a pin has to admit the auxiliary model the CLI bills beside the one the step asked for. The budget is per attempt: a retry starts a fresh one, and `retries` bounds how many;
  - `mill doctor`'s `gateway` probe makes one token count through the gateway — a request the upstream answers without billing — and fails when the credential file is absent or the upstream refuses it; it warns at eleven months from `model_credential_minted` in the configuration file, because the CLI's three-day expiry warning applies only to `/login` credentials (docs/tools.md §doctor).
- `HOME` stays on the environment floor for the shell, transcripts and the harness's own state — not for a login; `envFloor` and `ENVIRONMENT_PASSTHROUGH` say so.
- The clone at step 2 of the sequence uses the worker's forge credential on the host. No session is running; no session credential exists yet.

## Session identity

- Sessions run as `agent`, uid 1000, home `/home/agent`, not root. `--private-users=pick` maps uid 1000 into the machine's range; the host sees an unprivileged uid.
- `/work` is owned by `agent`. Commit identity and signing come from the workspace (#50), not from the session's shell.
- `/home/agent` is on the ephemeral root. Everything under it dies with the run except the cache lineages mounted into it.
- `~/.claude` is ephemeral, with the skill mounted read-only into it. The harness's own scratch under `~/.claude` is written and lost.
- Root exists for systemd and for the `dnf` shim. Nothing else escalates.
- A fixed uid rather than per-run: paths in documentation and `mill.toml` stay literal, and isolation does not depend on the uid — the user namespace is the boundary.

## Skill mount

- The mill skill and the sidecar are the worker's, versioned with the binary, and **MUST NOT** be baked into the image. The worker bind-mounts the skill read-only at `/home/agent/.claude/skills/mill` at provision; the sidecar is placed where the harness adapter expects it, by the same mechanism.
- The skill **MUST** document `mill.toml`: the `[image]` and `[cache.<name>]` tables, the path rules, the defaults by ecosystem, and that runtime `dnf` installs are not remembered.

## The dnf shim

- `/usr/local/bin/dnf` in the base image (`images/base/dnf`) wraps `sudo /usr/bin/dnf`, and the sudoers rule allows `agent` to run `/usr/bin/dnf install` and nothing else. The proxy goes to dnf as `--setopt=proxy=` after `install`, because sudo strips the environment and the rule's pattern is `dnf install *`.
- On success the shim prints, as its **last lines**, a hint: the packages installed, that this machine is ephemeral, and the `[image].packages` line to add to `mill.toml`. Agents read the tail of output; the hint is where they look.
- The shim reports the install over the workspace socket (`/run/mill/ws.sock`, a POST to the `dnf` verb preparatory steps add; a socket that is absent or does not answer costs nothing). The run's provenance records "installed `<packages>`, not declared in `mill.toml`". This is not routed to the reviewer, who judges acceptance criteria only; it is for the operator to see which projects keep needing something undeclared.
- Nothing installed this way survives the run. The declaration the agent commits is what is remembered.

## Preparatory steps

A preparatory step readies the machine for the sessions. It is a deterministic step with an exit code and output as evidence, and it has a time budget. Its output is recorded, not shown to a session, which keeps `mise install`'s pages of progress out of the session's context.

- **Image build** (§Derived images) — runs before boot. Failure → boot from base, finding to the first session.
- **Toolchain** — `mise trust /work && mise install`, first exec in the machine. Failure → the run continues into the first session with the last lines of output as a finding. The agent fixes `mise.toml`; the next run's toolchain step passes.
- **`mill.toml` validation** (§mill.toml) — failure → defaults, finding.

The rule: a preparatory step **MUST NOT** fail the run. The circuit breaker exists for work that cannot be done, not for a typo an agent can fix in one session.

Budgets are the workflow's. A preparatory step and a check step each run under the `timeout` the workflow definition gives them — `workflows/feature/workflow.yaml` already carries `20m`, `30m` and `168h` on its steps — not under any harness setting. Two consequences:

- Every preparatory step **MUST** have an explicit `timeout`; there is no default that is right for both a warm `mise install` and a cold Rust toolchain.
- **The sidecar MUST set the session's shell timeout to the step's budget.** It does, from the `timeout_ms` the adapter puts on every `Start` and `Resume` (`docs/architecture.md` §"Skill steps: the harness adapter"). Without it a session gets the SDK's default and a command over about two minutes is backgrounded — after which a session invents a poll loop and loses an hour, which is the failure the harness fixed for its own sessions (`mill#42`) before mill's. A session that can wait in the foreground never has a task to lose.

## Resource limits

- `MemoryMax`, `CPUQuota`, `TasksMax` on the machine unit bound the whole run, containers included, because the machine's cgroup tree is delegated from that unit.
- A workflow **MAY** declare per-step limits, set on the step slice.
- Disk is bounded by eviction, not by quota, in the first iteration. A run that fills the filesystem is the failure mode this leaves open.

## Privilege

The worker is not root. It runs as the operator's account, as a user unit — `contrib/systemd/mill.service`, deployed by homestead — because that account is mill's dagger principal and holds the forge token and the model credential, and none of that should be root. What needs root is starting a machine, and that is separated out the way `homestead-apply` already is: a system unit the operator's uid may drive through polkit.

- `mill-machine@.service` is a root-owned template on the system manager. Its `ExecStart` is the `systemd-nspawn` invocation of §Provision, reading the run's declared binds and limits from `/var/lib/mill/runs/%i/machine.json`, which the worker writes. The worker starts and stops it with `systemctl start mill-machine@<run-id>` under a polkit rule granting its uid `org.freedesktop.systemd1.manage-units` for that template and nothing else — homestead's `homestead-apply` rule, copied not widened.
- Exec into the machine is `systemd-run -M` as the operator's uid, under a polkit rule for `org.freedesktop.machine1.shell` and `manage-machines`. Whether machined's polkit path satisfies `systemd-run -M` for an unprivileged caller, or the namespace entry still wants `CAP_SYS_ADMIN`, is the spike's E0; it decides the shape of this section before anything else is built.
- If E0 fails, the fallback keeps the pattern: a small root-owned helper unit with three verbs — start, exec, stop, by machine name — that the polkit rule admits the operator to. Still a system unit plus a polkit rule; the worker still never holds root.
- Every process inside the machine is unprivileged on the host through the user namespace, including root inside a `nested` machine.
- Unprivileged nspawn through `systemd-nsresourced` (E7) would remove the template unit entirely. It is the more elegant answer and the less proven one; it stays a spike for repos without `nested`.
- The polkit actions the rules name are present on `core` (§The host).

## SELinux

`core` enforces SELinux. Nested labelling for a machine that runs containers needs either `--selinux-context=`/`--selinux-apifs-context=` on the invocation or a small local policy module; the spike (E4 and the nested experiments) decides which, and the answer goes here.

## External state

Processes are the easy half of cleanup: a slice reaps a step, a unit reaps a machine, an outer machine takes its inner machines with it. What leaks is everything a machine creates *outside* itself, and this repo has been bitten by that class three times in two days — an org-scope workflow row from one run failing the next (`mill#34`), sixty-nine `mill-acceptance-*` projects waiting for delayed deletion on the forge, scratch dagger projects that are archived only if a Ginkgo `DeferCleanup` happened to run.

The rule: **a machine may only create external state that is swept by name from the host side.** `DeferCleanup` is not a teardown mechanism when the process can be killed.

| External state | Named by | Swept by |
|---|---|---|
| Machines | `mill-<run-id>`; suites use `mill-acceptance-<scenario>-<nonce>` | worker reconciliation: any `mill-*` machine with no live run |
| Cache lineages | scope and name; nested runs get their own root | eviction, and the nested root dies with the outer's ephemeral root |
| Dagger scratch projects | `mill-acceptance-<scenario>-<nonce>` | worker reconciliation: archive any matching project older than the suite's budget. Dagger has no delete |
| Forge scratch projects | `mill-acceptance-<scenario>-<nonce>` in the scratch namespace | worker reconciliation: delete any matching project older than the suite's budget |
| Workflow rows a suite derives | project scope, in the scenario's scratch project | swept with the project; never at org scope from a suite |
| Run directories, `.building` images | run id, derived digest | worker reconciliation |

Reconciliation runs the sweeps on the same pass it reconciles machines. A suite that cannot name its state so that a sweep finds it has a bug in the suite, not a gap in the sweep.

## Reconciliation on resume

The ledger records per run: machine name, machine unit, derived digest, veth name, current step slice, lineages restored, and the candidate snapshot's step.

On worker start, in this order:

1. `machinectl list --output=json` and compare with live runs:
   - a machine with no live run is an **orphan** → stop its unit, delete its run directory, record the fact.
   - a live run with no machine is a **lost sandbox** → the run fails with that as evidence. Nothing resumes into a fresh machine; the workspace's state is not the ledger's to reconstruct.
   - a live run with its machine present resumes at the ledger's step after stopping and recreating the current slice. Whatever was running when the worker died is not trusted to still be the step. The candidate snapshot from the last evidence-closed step is still valid and still promotes at finish.
2. Delete `*.building` under `/var/lib/machines/` and any run directory with no live run.
3. Sweep external state by name (§External state): stale `mill-acceptance-*` dagger and forge projects.
4. Run eviction.

Polling `machinectl` is sufficient at run cadence. If it is not, host-side D-Bus signals are the upgrade, for lifecycle only; exec stays on the CLI.

## Failure modes

| Failure | Detection | Response |
|---|---|---|
| Base digest unresolvable (Harbor down) | the pull fails at sequence step 4 | Use the last resolved digest (`/var/lib/mill/images/base.json`) if its subvolume exists, recording the fallback; else fail the run before boot |
| Derived build fails or times out | `podman build` exit / budget | Boot base; finding to first session |
| `mill.toml` invalid | parse/validate at step 3 | Defaults; finding to first session |
| Machine never reaches ready | readiness timeout | Destroy, fail the run |
| Toolchain step fails | exit ≠ 0 | Continue; finding to first session |
| Exec unit fails to start (not the command) | `systemd-run` non-zero before the service runs | Retry once; then fail the step with stderr as evidence |
| Command exits non-zero | `--wait` propagates | The step's contract decides |
| Step exceeds its budget | mill's timer | Stop the slice; no candidate snapshot; fail the step |
| Machine dies mid-step | exec returns a connection error; `machinectl show` reports none | Fail the run: lost sandbox. Candidate from an earlier closed step still promotes |
| Worker dies mid-step | §Reconciliation | Resume at the ledger's step with a fresh slice |
| Proxy unavailable | connections from the machine fail | The step's contract decides; the worker's health check covers the proxy |
| Cache filesystem full | promotion or restore fails with ENOSPC | Run eviction, retry once; then the run finishes without promoting |
| Lineage evicted mid-run | absent at promotion | Promote into a fresh lineage |
| Run directory removal fails | error on destroy | Finish; log; reconcile next start |

## Testing

The behaviour is specified as scenarios in [sandbox-scenarios.md](sandbox-scenarios.md). Each is a Ginkgo spec named for the scenario, in one of three tiers the tree already uses:

- **unit** — against the fake backend, no machine. `make test` runs it.
- **db** — `Label("db")`, a testsupport database. `make test` runs it.
- **live** — `Label("live")`, a real machine on a host with root, nspawn, btrfs and Harbor. The spec skips in `BeforeEach` when `MILL_SANDBOX_HOST` is unset, and the skip message names what running it costs, in the words the existing `live` specs use: *"set MILL_SANDBOX_HOST=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine."* `make test` runs it and it skips; on the host it runs.

There is no fourth tier and no new target. What makes the live tier real rather than a skip nobody triggers is §Self-hosting.

**Every suite runs against a throwaway dagger.** `mill#51` moves the acceptance suite to the fixture in `internal/testsupport/dagger.go` by default: a blank database from the shared testcontainers Postgres, dagger's own migrations applied with `goose` from a dagger checkout, one seeded org and one `user`-kind principal bound to the spec process's uid, one `dg serve` on a temporary socket, `SIGKILL`ed on cleanup. Its criteria are that `make test-acceptance` with no socket variable creates nothing in the deployed dagger, that the fencing and resume scenarios pass against the disposable instance, and that a killed suite leaves nothing behind. Nothing the acceptance scenarios exercise — claim, heartbeat, fence, gate, complete, usage, holds — needs the deployed graph: fencing is per-lease and uses a second client under the same principal, and the seeded principal is `user`-kind, so it can answer a gate and place a hold.

The fixture is **per-spec** and stays that way: per-suite would share one graph across parallel scenarios, which is the cross-scenario pollution being removed. A spec that uses it carries the `db` label.

Two rules differ by tier:

- The **local-workspace suite** keeps `MILL_ACCEPTANCE_DAGGER_SOCKET` as a deliberate opt-in to the deployed dagger, for a `#38`-style proof on the deployment and for checking whether the real graph agrees with the throwaway one. The runbook says which is which.
- The **live tier refuses** any socket the environment names as the deployed dagger, flag or not. It starts machines through a root-owned unit and `mill run` subprocesses inside them, and `mill#49` already showed a test process becoming a worker against the live graph with no session involved. A flag a stray variable can flip is not a safe default for that tier.

**In a machine, the fixture's prerequisites are image or mount contents**, and neither `mill#51` nor the base image names them today: dagger's store migrations from its checkout, `goose`, and the `dg` binary. Until `dagger#25` publishes a dagger image with an exported fixture, the mill repo's machine gets the dagger checkout bind-mounted read-only, declared in its `mill.toml` the way the image subvolumes are for `nested`. That is a deliverable of the backend milestone.

If mill ever takes an `agent`-kind principal of its own — so dagger can enforce the gate rule against it — the fixture needs **two** seeded principals, the mill and the person, because today a gate is answerable only because both are the same one. `seedDagger` is a four-row insert; the change is trivial, the fact is not.

The live suite's machines are named `mill-acceptance-<scenario>-<nonce>` so that §External state's sweep finds them after a kill.

## Self-hosting

Mill builds mill, and the sandbox is the thing being built, so the sequence matters.

**The privilege wall.** Sessions in the build loop run as the mill account with no root. `systemd-nspawn` needs root. So an agent working a sandbox node on the local workspace cannot execute the live tier, and the reviewer — "a criterion you cannot verify is a fail" — cannot verify it either. Root inside a machine is a different matter (§Provision): the namespace is the boundary, and giving `agent` sudo there costs nothing the design has not already accepted.

**The sequence.**

1. **Build on local, gate once.** Agents implement the backend on the local workspace. The live specs exist, are labelled, skip with the right message, and assert the scenarios' Then clauses — that is what the reviewer checks, and a spec named for a scenario that asserts nothing is a fail. The milestone that creates the backend ends at exactly one `assignee_kind: user` node, shaped like `mill#38`: run `MILL_SANDBOX_HOST=1 make test` on `core` as root, complete the gate with the ginkgo summary as its result. The loop drains to `WAITING: nothing claimable` when only that node remains, which is a clean stop. Its acceptance criterion is the output, not a person's say-so, so that a skipped tier cannot be mistaken for a passing one during the window the backend is least trusted.
2. **Wire the selector.** `workspace.Kind` names `KindLocal`, `KindMachine` and `KindRemote`, and `run.Workspaces` — a function type — is chosen by it: `workspacesFor(...)` in `internal/cli/run.go` hands the loop `localWorkspaces(...)` or `machineWorkspaces(...)` according to `--workspace` on `serve`, `work` and `run`, or `workspace:` in the configuration file, with `local` the default. Moving to machines is that line of configuration once the `Workspace` implementation behind `machineWorkspaces` — `internal/workspace/machine`, built over its `Driver` seam by the lifecycle and exec nodes — exists; until it does, a worker configured for machines refuses before it claims, naming the node that builds the backend. The sequence names these so nobody writes "config change" and stops. The kind is `machine`, not a rootless container, which [ADR-018](adrs/018-a-runs-sandbox-is-a-systemd-nspawn-machine.md) rejects.
3. **Move the mill repo into machines with `nested`.** Worker config grants `[machine].nested` for the mill repo. From here the live tier runs inside the agent's own machine as a check step under the workflow's `timeout`, on every node that touches the sandbox, with an exit code as evidence. No skip, no gate, no `WAITING`.
4. **Other repos follow without `nested`.** They never boot machines; they get the default provision.

**Why the gate is once.** You cannot test the first machine from inside a machine. After the first, the sandbox tests the sandbox. One `#38`-style node, ever, is the price of the bootstrap; a gate per milestone was the price of not being willing to give root inside, and that price was never worth paying.

**Nested and unprivileged are in tension.** `nested` wants sudo and a wide uid range inside; the unprivileged-nspawn line (§Privilege, E7) wants no template unit outside. Whether an unprivileged outer can host an inner is a separate experiment, not a corollary of the two. Until it is run, E7 is scoped to repos without `nested`, and every machine is started by the root-owned template unit under polkit.

## Sequencing across repositories

The sandbox has prerequisites mill cannot provide for itself. In order:

1. **homestead — `mill_storage`.** The btrfs volume (§Storage). Nothing else can start without it, and the spike needs only this.
2. **The spike**, run on `core` by a person (§The host). E0 (polkit) and E13 (nested) decide the shape of §Privilege and §Self-hosting; E1–E6, E9–E11 accept the ADR.
3. **homestead — a sandbox ADR and the `mill` role.** Homestead's ADR names the third runtime class on the host: neither a Quadlet service nor a libvirt guest (homestead's ADR-006 rejected nspawn for a workload that *is* an OS; a run is not one), and it is what reconciles mill running under the operator's account with homestead's ADR-002, because a session inside a user namespace is not that account. The role is the one that deploys `contrib/systemd/mill.service` plus: `systemd-container`, `mill-machine@.service` and its polkit rule, the nftables table in `thicket_nftables`'s style (its own table, its own loader unit, never a flush), squid, the SELinux module in `thicket_selinux`'s style, and `vault_claude_code_oauth_token` delivered by `LoadCredential`. The homestead project's hold lifts for exactly this.
4. **mill — the backend milestone** ([Roadmap §O2](roadmap.md#o2--isolated)), on the local workspace: `mill#51` first (the whole suite on the throwaway dagger, with the in-machine prerequisites named), then the `Workspace` implementation and selector, images, lineages, the credential gateway, the environment-floor and sidecar-timeout fixes, the live tier. Ends at the one bootstrap gate.
5. **mill — sessions move into machines** with `nested` and the dagger checkout bound read-only. The sandbox tests itself from here.
6. **dagger — `dagger#25`**, a published image with an exported fixture, so the machine's dagger is a pull rather than a bind of a checkout; and a real delete for scratch projects, which is hygiene for the deployed path only — the throwaway instance's cleanup is `DROP DATABASE`.

The credential gateway sits in step 4's critical path: a machine with no `credentials.json` cannot run a session without it, so its spike (the setup-token upstream) runs alongside E0 in step 2.

## Findings against the tree

Things this design found that are wrong or missing today, each a node of its own:

- The sidecar sets no shell timeout (§Preparatory steps).
- `tests/acceptance` defaults its dagger socket to the deployed graph (`mill#51`); the live sandbox suite must not inherit that (§Testing).
- Suite cleanup is `DeferCleanup` only; nothing sweeps scratch dagger and forge projects or org-scope workflow rows by name (§External state).
- The testsupport dagger fixture needs a dagger checkout, `goose` and `dg` on the host; in a machine those are mount or image contents nobody has named (§Testing).

## Open questions

- Whether `--private-users-ownership=auto` behaves on the workspace bind under SELinux enforcing. E4.
- Whether podman containers land under the step slice without `--cgroups=split`. E3.
- Whether testcontainers' reaper is content with `podman.socket` inside the machine. E6.
- Whether the derived-image export should snapshot base and `dnf --installroot` instead of `podman build` + flat export. Measured by the storage spike (E10: build 4.3 s, export 0.8 s, a derived subvolume the size of base plus the package) and closed: the flat export stays. Exporting onto a snapshot of base is the change to make if `mill image ls` ever shows dozens of derived images.

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
