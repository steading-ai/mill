---
status: proposed
date: 2026-09-10
tags: [mill, adr, sandbox]
---

# A run's sandbox is a systemd-nspawn machine

Would be `018` in `docs/adrs/`. Companion documents: [[Per-Run Sandbox (Tech Spec)]], [[Per-Run Sandbox (Scenarios)]] and [[2026-09-10 - Spike - nspawn per-run sandbox]].

## Context and Problem Statement

Every step of a run drives a coding-agent session inside a workspace, and every process a step spawns goes through `Workspace.Exec`. Nothing yet decides *what* that workspace is a boundary of. The sessions run frontier models against projects the operator defines; the system is single-tenant today and may become multi-tenant later.

The sandbox must be a full Linux environment, because the workload includes the project's own test suite, and that suite runs containers through podman. Projects are polyglot — Go, Rust, Node and Bun, Ruby — with toolchains managed by mise, and some need system packages mise cannot provide. Which backend provides one sandbox per run, can run containers inside it, keeps the run's credentials and filesystem away from the host, makes a warm toolchain and build cache cheap, and does not foreclose the move to a stronger boundary when the tenant model changes?

## Decision Drivers

- **Core principle 3** — `Workspace.Exec` is the single chokepoint and the local → container → remote swap is a config change. The backend must sit entirely behind that interface.
- **Core principle 8** — evidence closes steps. Mill must be able to run a check inside the run's sandbox itself, independently of the agent's session, and read a real exit code. Anything that persists across runs must persist because a step closed on evidence.
- **Core principle 9** — a session never sees a credential it could keep. Credentials are step-scoped; the sandbox is run-scoped; nothing a step spawns may survive the step.
- **Core principle 2** — one node's worth of state per run. The sandbox must leave nothing behind except what was deliberately promoted.
- **Nested containers are the primary workload**, not an edge case. This is the distinction between a system container (real init, delegated cgroups, full user namespace) and an application container.
- **Threat model is containment, not adversary.** A wrong `rm`, a test that binds a port, a dependency that scribbles in `$HOME`, an agent reading the operator's other checkouts. A kernel exploit is out of scope until multi-tenancy is in scope.
- **The worker is the remote piece.** Mill has two faces; the worker runs wherever the sandboxes run. The sandbox backend never needs a network API of its own.
- **Least new machinery.** Mill stores nothing but the record of its own runs; a second stateful control plane is a cost.
- **Polyglot projects under mise**, occasionally needing system packages the operator does not want to hand-maintain per project.
- **Host facts** — Fedora 44, systemd 259 with `systemd-nspawn` and `systemd-vmspawn`, SELinux enforcing, rootless podman 5.8, bare metal with `/dev/kvm`, btrfs, and a Harbor registry with proxy-cache projects.

## Considered Options

1. systemd-nspawn, one machine per run
2. Incus, one instance per run
3. Rootless podman, one container per run, podman nested inside
4. A microVM per run (Firecracker or Cloud Hypervisor, driven directly)
5. Podman with the `krun` runtime (a microVM behind the podman CLI)
6. gVisor (`runsc`)
7. bubblewrap / raw namespaces without an image
8. A hosted sandbox service (Fly Machines, E2B, Modal)
9. Kubernetes, one pod per run

## Decision Outcome

Chosen option: **systemd-nspawn, one machine per run**, because it is the option with the least new machinery that treats "runs containers inside" as the normal case rather than a trick, and because its exec channel, step reaping, ephemeral filesystem, egress control and resource limits all come from systemd rather than from code mill writes. btrfs makes the rest of the design — ephemeral roots and cache lineages restored by snapshot — a matter of O(1) operations.

The shape of the decision:

- A run's sandbox **MUST** be a machine registered with `systemd-machined`, started as a transient unit on the host and stopped with it. The machine's name is the run's identity in the sandbox layer.
- The machine **MUST** boot from an ephemeral btrfs snapshot of an image subvolume; nothing written to the root filesystem survives the run.
- Images are two layers: a **base** the operator owns, built from a Containerfile in the mill repo and published to Harbor; and a **derived** image per repo, base plus the system packages the repo's `mill.toml` declares, built on the worker on first use and identified by `hash(base digest, packages)`.
- `Workspace.Exec` **MUST** be realised as a transient unit inside the machine (`systemd-run -M`), with streamed stdio, working directory, uid, environment and exit code carried by the unit. Every step **MUST** run under its own slice, and ending a step **MUST** stop that slice.
- Caches **MUST** be btrfs lineages: named directories whose state is carried between runs as read-only snapshots, restored by snapshot into the run and promoted only from the last step that closed on evidence. Lineages are looked up by name and scope, not by key; eviction is by expiry and free-space watermarks.
- Container images **MUST** come through Harbor's proxy-cache projects; the machine's own container store stays ephemeral.
- The machine **MUST** have its own network namespace on a host-side veth, and all egress **MUST** pass through a forward proxy on the host, enforced by host nftables rather than by anything inside the machine. The first proxy is transparent in policy — no allowlist, no interception — so the architecture is fixed while the policy is not.
- Credentials **MUST NOT** be placed in the machine's environment or filesystem. Git, forge and registry credentials are minted over the workspace socket against the step token. **The model credential is held by a host-side gateway**: the session's `ANTHROPIC_BASE_URL` points at it, the session's `ANTHROPIC_AUTH_TOKEN` *is* the step token, and the gateway swaps in the subscription credential (a `claude setup-token`, held on the host, delivered by `LoadCredential`) on the way out. The machine's `~/.claude` holds no login. The gateway also sees every request, which gives usage per step from outside the session and makes a step's cost budget enforceable.
- The worker **MUST NOT** run as root. It is the operator's user unit, because that account is mill's dagger principal and holds the forge token and the model credential. Starting a machine is the one operation that needs root, and it is a root-owned `mill-machine@.service` template the operator's uid drives through polkit — the `homestead-apply` pattern, copied not widened.
- Sandbox storage — lineages, run directories, image subvolumes — lives on a btrfs volume of its own (`nvme1n1` on the reference host, LUKS + btrfs, `user_subvol_rm_allowed`), because the host's root is XFS. Nothing in the cache design runs on XFS as written; reflink is the fallback if the volume ever goes.
- Preparatory steps — image build, toolchain install, `mill.toml` validation — **MUST NOT** fail a run; their failure is a finding for the first session.
- A repo whose tests boot machines **MAY** request `[machine].nested`; granted per repo by worker config, it gives the machine a wide uid range, `sudo` for the session user, and the host's images read-only. Root inside a user namespace is an unprivileged uid on the host; the namespace is the boundary, not the uid. This is what lets the sandbox test itself after one bootstrap gate.
- A machine **MUST NOT** create external state — dagger projects, forge projects, workflow rows, lineages — that the host cannot sweep by name. A suite's `DeferCleanup` is not a teardown mechanism when the process can be killed.
- Mill **MUST** drive systemd through its command-line tools, not D-Bus. The operation mill performs most — exec into a machine — has no D-Bus path from Go without a helper process; the CLI inherits libsystemd's namespace-entry logic and its fixes. D-Bus **MAY** be added later for host-side lifecycle events only.
- The worker **SHOULD** run as root for the first deployment; the payload is unprivileged through the machine's user namespace. Unprivileged nspawn via `systemd-nsresourced` is a spike, not a dependency.

### Consequences

- **Good**: The exec channel, step reaping, cancellation, resource limits and journal integration exist already; mill shells out to them through its existing chokepoint.
- **Good**: The boundary is the same kernel plus the init system the host already runs. No daemon, no storage pool, no second database to reconcile the ledger against; `machinectl list` is the truth.
- **Good**: Podman inside is ordinary podman — real init, delegated cgroups, native overlay in a user namespace. No `fuse-overlayfs`, no subuid juggling, no socket forwarded from the host.
- **Good**: Restoring a multi-gigabyte build cache is a snapshot, not an untar. Caching is a hot path, not a tax, and a promoted generation exists because a step closed on evidence.
- **Good**: The operator maintains one base image. A project's extra packages are a line in its own repo, built lazily, and a mistake in that line degrades to a finding the agent fixes.
- **Good**: `systemd-vmspawn` boots the same tree under QEMU, registers with the same machined, and answers to the same `-M` addressing. When multi-tenancy demands a hardware boundary the boot command changes and the interface does not.
- **Good**: Egress topology lives in host nftables on a veth the host owns, which is a stronger position than anything a container runtime offers from inside. Policy, when it comes, is a change to the proxy's configuration and nothing else.
- **Bad**: A shared kernel. An unprivileged process in a user namespace has more kernel surface than a VM guest. Acceptable under the containment threat model and exactly the thing the vmspawn path exists to change.
- **Bad**: A root-owned template unit and a polkit rule are host configuration mill cannot ship or test on its own; they are a homestead role, and the spike's first experiment (E0) has to prove `systemd-run -M` works for the operator's uid through polkit before the shape is final. If it does not, a small root helper unit takes its place — the same pattern, one more moving part.
- **Bad**: A second disk. The sandbox's storage is a volume the host did not have, provisioned and encrypted by a homestead role, and the design's O(1) claims are true only on it.
- **Bad**: A gateway is new code in the worker — a streaming reverse proxy — and the setup-token upstream is a spike (E14) until the headers the upstream wants from a bearer subscription token are known. The fallback puts the one-year token in the session's environment, which is short of principle 9 but a different order of harm from a refresh token on disk.
- **Bad**: SELinux enforcing on the host will cost a policy adjustment for nested labelling. The spike measures how much.
- **Bad**: Linux, systemd and btrfs only. There is no macOS story, none on a host without systemd, and the cache design assumes a CoW filesystem. Mill's worker was never going to run anywhere else, but the constraint is now explicit.
- **Bad**: Derived images are flat exports; variants do not share extents with the base. A couple of GB per project variant, bounded by eviction.
- **Neutral**: The base is a Containerfile, so iterating on it is a `podman build`. The tree is what nspawn boots; how it was produced does not matter to it.
- **Neutral**: Killing the `systemd-run` client does not stop the unit it started. Cancellation is by stopping the slice by name, which is what step teardown does anyway.

## Rejected Options

### Incus, one instance per run

The other serious system container. A Go daemon with a REST API, image server, network ACLs, and `--vm` on the same instance type.

- **Good**: Exec over websocket with streamed stdio is `Workspace.Exec` as an API call. Container now, VM later, is literally a flag.
- **Bad**: A root daemon with its own state — pools, images, instances — beside a mill that stores nothing but its own record. Resume has to reconcile the ledger against incusd's database instead of against transient units that die with their process. Two control planes where nspawn adds none.

### Rootless podman with podman nested inside

The documented `quay.io/podman/stable` pattern: `--device /dev/fuse`, `--security-opt label=disable`, an inner subuid range carved from the outer one, `fuse-overlayfs` as the inner driver.

- **Good**: No privilege on the host. Podman is already the one assumed system package.
- **Bad**: A weaker boundary than plain podman — the run gets `/dev/fuse` and a nested user namespace — in exchange for the tarpit of `fuse-overlayfs` under a compiler, SELinux label disabling, and a socket the test suite's reaper expects. It is the thing people do until it burns them, and it burns on this workload.

### A microVM per run, driven directly (Firecracker, Cloud Hypervisor)

The boundary every multi-tenant CI executor converged on.

- **Good**: A real kernel per run. Nested containers are not nested. The adversarial threat model is covered on day one.
- **Bad**: Kernel and rootfs pipeline, an in-guest agent, a vsock exec protocol, tap networking — about a week of work to buy a boundary the current tenant model does not need. And a host constraint: KVM, so bare metal or nested virt. Kept as the direction, through vmspawn, not as the first backend.

### Podman with the `krun` runtime

A microVM behind the podman CLI.

- **Good**: The same invocation as podman, with a hardware boundary.
- **Bad**: Podman *inside* a `krun` guest is the specific combination without a track record; the guest rootfs is virtiofs and inner storage needs a block-backed volume; `crun-krun` is not installed. Too much bet on too little prior art.

### gVisor

- **Good**: A userspace syscall boundary without a VM.
- **Bad**: Does not host a nested container runtime usefully, and taxes exactly this workload — compilers, git, test suites are syscall storms.

### bubblewrap and raw namespaces

- **Good**: Fast, no image, no daemon.
- **Bad**: No init, no cgroup delegation for a nested runtime, no image means no reproducible environment.

### A hosted sandbox service

- **Good**: Someone else's isolation, real multi-tenancy, Fly Machines takes an OCI image and returns a VM.
- **Bad**: Remote-only, so a local backend is still needed for the acceptance suite, so two backends get written. The credential path crosses a vendor.

### Kubernetes

- **Good**: The mature answer at fleet scale.
- **Bad**: A cluster on the repo's third day, and local development coupled to one. Nested containers in a pod need privileged or a special runtime, which is the same problem moved somewhere more expensive.

### Rejected within the chosen option

- **Keyed caches** (lockfile-hash keys, GitHub-Actions style). Keys there serve lookup in a flat blob store, branch isolation for untrusted contributors, and size control. A btrfs lineage answers lookup by name; the tenant model has no untrusted contributors; size is a watermark. Every cache mill restores is self-invalidating on correctness — Go's build cache, cargo's fingerprints, npm's content-addressed store, bundler's lockfile reconciliation — so a key would defend against a problem the tools already solve. An optional per-lineage `key` remains for a tool that is not self-invalidating; the defaults do not use it.
- **OCI-stored caches in Harbor as the hot path.** Per-run tar, compress, upload, download, untar of whole caches is the slowest stage of the pipelines that do it. Kept as a possible second tier for a second worker; not built now.
- **Snapshotting the whole machine as the cache.** The pet-VM antipattern: everything a run wrote persists, and the ephemeral root stops being ephemeral. Named paths are snapshotted; the machine is disposable.
- **Baking the mill skill and sidecar into the base image.** They are the worker's and change with its version; baked in, a worker upgrade means a base rebuild and every derived image behind it. They are mounted at provision instead.

## More Information

- [[Per-Run Sandbox (Tech Spec)]] — the sequence of a run, images, `mill.toml`, lifecycle, caching, eviction, network, credentials, identity, reconciliation.
- [[Per-Run Sandbox (Scenarios)]] — the behaviour as Gherkin, the contract the specs are written against.
- [[2026-09-10 - Spike - nspawn per-run sandbox]] — the experiments that must pass before this ADR is accepted.
- Related repo decisions: `docs/adrs/012-workspace-exec-is-the-chokepoint.md`, `004-evidence-closes-steps.md`.

Revisit when any of these change:

- **The tenant model.** A second tenant, or a project the operator does not control, moves the threat model to adversarial and the boot command to `systemd-vmspawn`. Cache lineages become per-tenant. A workflow that boots a guest inside the machine (the tech spec's §Future considerations) becomes nested virtualisation under vmspawn; weigh that cost when the time comes.
- **The unprivileged spike passes.** If nspawn under `systemd-nsresourced` gives veth networking, `-M` entry, and nested podman, drop root from the worker for repos without `nested`. Whether an unprivileged outer can host an inner machine is a separate experiment.
- **The nested spike fails.** If nspawn cannot host nspawn on this kernel, the live tier stays a host-run gate per milestone and `[machine].nested` leaves the spec.
- **Resume needs push.** If polling `machinectl` proves inadequate, add D-Bus for host-side lifecycle signals only.
- **A second worker.** Derived images rebuild there; if that or cold caches prove costly, Harbor becomes the second tier for cache generations.
- **LRU thrashes.** Consult dagger's ready set to protect repos with pending work from eviction.
