---
status: proposed
date: 2026-09-10
---

# A run's sandbox is a systemd-nspawn machine

## Context and Problem Statement

Every process a step spawns goes through `Workspace.Exec`, but nothing decides what the workspace is a boundary of ([Architecture §The workspace](../architecture.md#the-workspace)). The workload is the project's own test suite, which runs containers through podman, on toolchains under mise. Which backend gives one sandbox per run, runs containers inside it, keeps credentials and the host out, makes a warm cache cheap, and admits a stronger boundary later?

## Decision Drivers

- [Core principle 3](../core-principles.md#3-every-spawned-process-goes-through-the-workspace): the backend sits entirely behind `Workspace.Exec`; local → machine → remote is configuration
- [Core principle 8](../core-principles.md#8-evidence-closes-steps-agents-dont): mill runs checks inside the sandbox and reads a real exit code; nothing persists across runs except from an evidence-closed step
- [Core principle 9](../core-principles.md#9-a-session-never-sees-a-credential-it-could-keep): credentials are step-scoped; nothing a step spawns outlives it
- Nested containers are the normal case: real init, delegated cgroups
- Containment, not an adversary, is the threat model until multi-tenancy
- Least new machinery: mill stores only the record of its own runs
- The host, `core`, is fixed ([Sandbox §The host](../sandbox.md#the-host))

## Considered Options

1. systemd-nspawn, one machine per run
2. Incus, one instance per run
3. Rootless podman with podman nested inside
4. A microVM per run
5. Podman with the `krun` runtime
6. gVisor
7. bubblewrap or raw namespaces
8. A hosted sandbox service
9. Kubernetes, one pod per run

## Decision Outcome

Chosen option: **systemd-nspawn, one machine per run**. It treats "runs containers inside" as ordinary, and exec, step reaping, ephemeral roots and resource limits come from systemd rather than from code mill writes. [`docs/sandbox.md`](../sandbox.md) is the specification; the shape is:

- One machine per run, booted from an ephemeral btrfs snapshot of an image — the operator's base plus the packages the repo's `mill.toml` declares — registered with `machined` by run.
- Started by a root-owned `mill-machine@.service` template the operator's uid drives through polkit; the worker never holds root.
- `Workspace.Exec` is `systemd-run -M` under a per-step slice; stopping the slice reaps the step.
- Caches are btrfs lineages, restored by snapshot, promoted only from the last evidence-closed step.
- A private network namespace on a host-owned veth; egress only through a host proxy, enforced by a static nftables table.
- No credential in the machine: git and forge over the workspace socket; the model through a host gateway that swaps the step token for the subscription credential.
- Preparatory steps degrade into findings, not failed runs.
- Mill drives systemd through its command-line tools, not D-Bus.

### Consequences

- **Good**: Exec, reaping, cancellation and limits exist already; `machinectl list` is the truth; no second control plane.
- **Good**: Podman inside is ordinary podman; restoring a multi-gigabyte cache is a snapshot.
- **Good**: `systemd-vmspawn` boots the same tree under the same addressing; multi-tenancy changes the boot command, not the interface.
- **Bad**: A shared kernel — acceptable under containment, and what vmspawn exists to change.
- **Bad**: Host configuration mill cannot ship — template unit, polkit rule, nftables, proxy, second disk — is homestead's; the spike must prove `systemd-run -M` under polkit before the shape is final.
- **Bad**: The gateway is new code and its upstream is a spike; the fallback is a one-year token in the session, short of principle 9.
- **Bad**: Linux, systemd and btrfs only.

## Rejected Options

### Incus

- **Good**: Exec over websocket; container now, VM later, is one flag.
- **Bad**: A root daemon with its own state for resume to reconcile against; a second control plane.

### Rootless podman with podman nested inside

- **Good**: No host privilege; podman is already assumed.
- **Bad**: `/dev/fuse`, `fuse-overlayfs` under a compiler, SELinux labels disabled; it burns on this workload.

### A microVM per run

- **Good**: A real kernel per run; the adversarial model covered from day one.
- **Bad**: Kernel pipeline, guest agent, vsock protocol: a week of work for a boundary the tenant model does not need. Kept as the direction, through vmspawn.

### Podman with `krun`

- **Good**: The podman CLI with a hardware boundary.
- **Bad**: Podman inside a `krun` guest has no track record.

### gVisor

- **Good**: A userspace syscall boundary.
- **Bad**: Hosts no nested runtime and taxes syscall storms: compilers, git, tests.

### bubblewrap and raw namespaces

- **Good**: Fast, no image, no daemon.
- **Bad**: No init, no cgroup delegation, no image.

### A hosted sandbox service

- **Good**: Someone else's isolation.
- **Bad**: Remote-only, so a local backend is still needed; credentials cross a vendor.

### Kubernetes

- **Good**: The answer at fleet scale.
- **Bad**: A cluster on the repo's third day; nested containers need privileged pods.

### Within the chosen option

- **Keyed caches**: every cache restored is self-invalidating; a lineage's name is its lookup.
- **OCI-stored caches in Harbor**: tar and untar per run is the slowest stage of pipelines that do it.
- **Snapshotting the whole machine**: the pet-VM antipattern.
- **Baking the skill and sidecar into the image**: they are the worker's, so they are mounted.

## More Information

Revisit when the tenant model changes (vmspawn, per-tenant lineages), when the spike settles unprivileged or nested nspawn, when resume needs push rather than polling, or when a second worker makes cold caches costly. Related: [ADR-004](004-evidence-closes-steps.md), [ADR-012](012-workspace-exec-is-the-chokepoint.md), [ADR-013](013-sessions-under-the-permission-classifier.md).
