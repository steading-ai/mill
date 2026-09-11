---
name: mill
description: Use when a question is about a mill run — what is running, why a run is stuck, what a step cost, what evidence closed it, whether this host could work at all — or about the machine a mill session runs in: what mill.toml declares, why a package installed with dnf is gone next run, why a connection hangs. Covers the mill MCP tools and the `mill` CLI, what each answers, which of them a session is allowed to call, the mill.toml grammar, and how a session reaches out.
when_to_use: mill status, mill inspect, mill explain, mill logs, mill cost, mill doctor, why is the run stuck, which step is it on, what evidence closed the step, a parked run, a waiting run, a mill workspace, a workflow version, mill.toml, image packages, a cache lineage, dnf install, HTTPS_PROXY, ANTHROPIC_BASE_URL, a connection that times out, nested machines. This is the mill workflow runner over a dagger work graph.
allowed-tools:
  - Bash(mill status:*)
  - Bash(mill inspect:*)
  - Bash(mill explain:*)
  - Bash(mill logs:*)
  - Bash(mill history:*)
  - Bash(mill cost:*)
  - Bash(mill workflow list:*)
  - Bash(mill workflow show:*)
  - Bash(mill workspace show:*)
  - Bash(mill doctor:*)
  - Bash(mill version:*)
  - mcp__plugin_mill_mill__status
  - mcp__plugin_mill_mill__inspect
  - mcp__plugin_mill_mill__explain
  - mcp__plugin_mill_mill__logs
  - mcp__plugin_mill_mill__history
  - mcp__plugin_mill_mill__cost
  - mcp__plugin_mill_mill__workflow_list
  - mcp__plugin_mill_mill__workflow_show
  - mcp__plugin_mill_mill__workspace_show
  - mcp__plugin_mill_mill__doctor
---

# Mill

**Autonomy:** model-invocable · reads a run unprompted · `cancel`, `rerun` and
`workspace_clean` are the session's own calls, never pre-authorized here

Mill is a workflow runner over dagger's work graph. It claims a ready node,
drives each step as a headless coding-agent session or a deterministic check,
closes a step only on evidence it verified, files a dagger gate when a person
must decide, and completes the node. A **run** is one dagger node being worked.

## Two rules before anything else

**Want work done? File a dagger node.** `run` is not an MCP tool and will not
be: it executes in the calling process, on the mill's own host and account. A
session that wants something built files it in dagger — the dagger skill is
how — and a mill worker picks it up. Do not try to start a run.

**The role decides, and nothing you say changes it.** Identity on mill's
socket is the peer uid of the process this server runs as, mapped by
configuration to a role. Every tool here reads except three: `cancel`,
`rerun` and `workspace_clean` change something, and a `reader` uid is refused
naming the role. Ask the operator; do not work around it.

| Role | May |
|---|---|
| the mill's own uid | Everything |
| `operator` | Read, and `cancel`, `rerun`, `workspace_clean` |
| `reader` | Read |

## Reading a run

A run is named by its dagger node reference — `mill#18` — or by the run id
`status` prints. A node reference resolves to that node's most recent run.

`status` → `explain` → `inspect` → `logs`.

| Tool | Answers |
|---|---|
| `status` | What is live, which step each is on, elapsed, cost, the gate a waiting run sits on. Start here |
| `explain` | Where one run is and **what it is waiting on**. Reach for this when asked why a run is stuck |
| `inspect` | Every attempt at every step, in order, with the evidence that closed it and how its process ended |
| `logs` | One attempt's log: the session's events, one summary line each, as the redactor wrote them |
| `history` | The audit trail — what changed, when, under which worker. The record of a run that has ended |
| `cost` | A window of usage rolled up by run, workflow, step and model. Counts, never currency |
| `doctor` | Whether this host could work at all: the socket, the schema, dagger, the sidecar, the signing key, the forge credential, the notifier |

The readers take `view`: `refs` is identifiers, `summary` the default, `full`
everything the ledger holds. Start narrow.

**Nothing here returns transcript content.** The ledger holds evidence,
identifiers and usage — not what a model said. `logs` is the one place a
session's own output surfaces, and it was redacted at write time.

## A stuck run is usually a person's turn

A run that is `waiting` or `parked` is blocked on a **dagger gate**, and only
a person completes a gate. `explain` names it and gives its title. Say what
you think should happen — comment on the gate in dagger if you can reach
it — and stop. Do not try to unblock the run yourself.

`parked` means the circuit breaker tripped: a step used its retries, or the
mill refused to guess. `explain` carries `parked_reason`.

## Changing something

- `cancel` asks the worker holding a run to stop the step it is on. The run is
  not failed: it keeps its workflow and branch and the next claim resumes there.
- `rerun` redoes a run from a named step, keeping the generation, the branch
  and the sessions. A failed run is reopened by it; an abandoned one is not.
  `restart`, which replaces all three, is CLI-only and not offered here.
- `workspace_clean` removes the checkouts of runs that have ended. `dry_run`
  first.

## The machine you are in

A mill session runs inside a machine of its run's own: an ephemeral
`systemd-nspawn` snapshot of an image, as `agent` (uid 1000, home
`/home/agent`), with the checkout at `/work`. The machine is discarded when
the run ends. What survives a run is what the run **promotes** — the caches
below, from the last step that closed on evidence — and what the session
**commits**. Everything else under `/home/agent`, `/tmp` and the root is
gone.

This skill is mounted read-only at `~/.claude/skills/mill`; the rest of
`~/.claude` is yours to write. `VERSION` beside this file is the version of
the mill worker that started the run, as `mill version` prints it.

## mill.toml

`mill.toml` at the repository root, beside `mise.toml`, is how a repo
declares what its machine needs. **Absent means defaults.** It is read from
the checkout when the run is provisioned, so a change to it takes effect on
the **next** run, not this one.

```toml
[image]
packages = ["libpq-devel", "vips-devel"]

[cache.go-build]
path = "~/.cache/go-build"

[cache.target]
path = "/work/target"
key  = ["rust-toolchain.toml"]   # optional

[cache.go-mod]
enabled = false                  # opt out of a default

[machine]
nested = true                    # only for a repo whose tests boot machines
```

Every key the parser accepts, and nothing else — an unknown key at any
depth is a finding naming it and is ignored:

| Key | Meaning |
|---|---|
| `image.packages` | Distro (Fedora) package names the image carries beyond the base. Each must match `^[A-Za-z0-9._+-]+$`; the list is sorted and deduplicated. Nothing else about the image is configurable from the repo |
| `cache.<name>.path` | Where the machine sees the cache lineage `<name>`. Absolute, or `~`-relative to `/home/agent`; see the path rules |
| `cache.<name>.key` | Optional list of files in the checkout, relative to its root. The lineage restores only when their hash matches the hash recorded at its last promotion; otherwise the cache starts empty. For a tool whose cache is not self-invalidating. The defaults set none |
| `cache.<name>.enabled` | `false` disables a default lineage. Absent reads as `true` |
| `machine.nested` | Requests that this repo's machine may boot machines of its own — for a repository whose test suite runs `systemd-nspawn`. It is a request; the worker's configuration grants it per repository, and an ungranted request is a finding and the machine boots without it |

### Path rules

- `cache.<name>.path` **must** resolve under `/home/agent` (`~`) or `/work`.
  It may not be `/work` itself, `/home/agent` itself, or `/run/mill`.
- `~` expands to `/home/agent`. The path is cleaned before the rules apply,
  so `/work/../etc` is `/etc` and refused. A relative path is a finding.
- `<name>` must match `^[A-Za-z0-9][A-Za-z0-9._-]*$`: it becomes a path
  component on the host.
- `key` entries are relative to the checkout root; an absolute path or one
  that escapes the checkout is a finding.
- Scope is not declarable. A cache with a default's name keeps the default's
  scope (and its path, when the declaration omits `path`); a new name is
  scoped to this repository. The worker, not the repo, decides what is
  shared across repositories.
- A `[cache.<name>]` with a finding is dropped whole; the rest of the file
  still applies. A file that does not parse is one finding and the run gets
  the defaults.

### Ecosystem defaults

The default caches come from the `[tools]` table of `mise.toml`. Names are
stable so `mill.toml` can override or disable them.

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

A global lineage is shared by every run on the worker because its tool
verifies what is in it; a repo lineage is trusted only within this
repository, and only from a step that closed on evidence.

### A `dnf install` is not remembered

`dnf install <package>` works in the machine — the shim runs it with the
proxy set — and its last lines say so: the machine is ephemeral and the next
run's image will not have the package. **Do not** leave it at that. Declare
the package and commit the declaration:

```toml
[image]
packages = ["vips-devel"]
```

The next run builds a derived image with it. The same goes for a cache a
tool keeps somewhere the defaults do not cover: declare a
`[cache.<name>]` for it rather than re-downloading every run.

Findings about `mill.toml` — an unknown key, a bad path, an ungranted
`nested` — reach the first session of the run as one line each, prefixed
`Provisioning:`. The agent that wrote the file is the one that fixes it.

## The network

**Every outbound connection goes through the proxy.** `HTTPS_PROXY` and
`HTTP_PROXY` name it in your environment, and `NO_PROXY` lists what
bypasses it. The host drops every other packet the machine
sends: a direct connection that ignores those variables does not get
refused, it **times out, by design**. So a connection that hangs is almost
never a network fault — it is a tool that did not read the proxy from the
environment. Check that first; do not work around it by retrying, changing
DNS, or looking for another route, because there is none.

- Tools that honour the standard variables — `curl`, `git`, `go`, `pip`,
  `npm`, `cargo`, `dnf` through the shim, `podman` pulls — just work.
- A tool that ignores them needs to be told: its own proxy setting, pointed
  at `$HTTPS_PROXY`.
- Container images resolve through Harbor's proxy-cache projects; a pull
  of `docker.io/library/postgres` is a LAN fetch the second time.

**The model is reached through the gateway.** `ANTHROPIC_BASE_URL` names it
and `ANTHROPIC_AUTH_TOKEN` is the step token, the session's only credential.
There is no login file in the machine and no API key. The token is the
step's: it is retired when the step ends and the next step's session is
issued its own, so nothing in a transcript is reusable.

## References

- [references/tools.md](references/tools.md) — every tool with its arguments
  and an example response, checked against `mill mcp --describe`
