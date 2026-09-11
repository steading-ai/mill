---
status: draft
date: 2026-09-10
tags: [mill, spec, sandbox, gherkin]
---

# Per-Run Sandbox — Scenarios

The behaviour in [[Per-Run Sandbox (Tech Spec)]] as Gherkin. Each scenario is a contract: it becomes a Ginkgo spec whose name is the scenario text and whose assertions are its Then clauses. Tags map to the three tiers the tree already uses: `@unit` runs against the fake backend with no machine; `@db` is `Label("db")` and takes a testsupport database; `@live` is `Label("live")` and boots a real machine — it skips in `BeforeEach` unless `MILL_SANDBOX_HOST` is set, with a message naming what it costs, and runs in the loop once the mill repo's sessions run in a machine with `[machine].nested`. Until then it is run once on the host by a person, on the record (spec §Self-hosting).

Vocabulary follows the spec: *machine*, *machine unit*, *step slice*, *exec*, *lineage*, *generation*, *promotion*, *preparatory step*, *finding*.

## Feature: The sequence of a run

```gherkin
Feature: The sequence of a run
  Configuration is read before the thing it configures exists, and nothing persists except through promotion.

  Background:
    Given a worker with a resolvable base image
    And a node whose repository the worker can clone

  @live
  Scenario: A run's configuration is read from its own checkout before the machine boots
    Given the repository's default branch has a mill.toml declaring package "libpq-devel"
    When the worker claims the node
    Then the workspace is cloned on the host before any machine exists
    And the derived image for "libpq-devel" is ensured before the machine boots
    And the machine boots from that derived image

  @live
  Scenario: The clone uses the worker's credential, not a session's
    When the worker clones the workspace
    Then no step token has been issued yet
    And the forge sees the worker's principal perform the clone

  @live
  Scenario: A run leaves nothing behind
    Given a run that wrote to /, /home/agent, /tmp and /var/lib/containers inside its machine
    When the run finishes
    Then the machine is no longer registered with machined
    And the run directory no longer exists
    And the only new subvolumes on the worker are promoted generations

  @unit
  Scenario: A run is not finished while its machine is registered
    Given a run whose machine unit is still active
    When the run loop attempts to mark the run finished
    Then it refuses
    And the reason names the machine
```

## Feature: Machine lifecycle

```gherkin
Feature: Machine lifecycle

  @live
  Scenario: A machine is a transient unit that boots an ephemeral snapshot
    When a run provisions its machine
    Then a host unit named "mill-run-<run-id>.service" is active
    And machinectl lists a machine named "mill-<run-id>" in state "running"
    And a btrfs snapshot of the image subvolume exists for the duration of the machine

  @live
  Scenario: Readiness is systemd's, not a sleep
    When a run provisions its machine
    Then the first exec waits for "systemctl is-system-running --wait" inside the machine
    And no step exec is issued before it returns

  @live
  Scenario: A machine that never becomes ready fails the run with evidence
    Given an image whose init cannot reach the running state
    When a run provisions its machine
    Then the readiness timeout elapses
    And the machine unit is stopped
    And the run fails with the readiness timeout and the image digest as evidence

  @live
  Scenario: Two machines never share a uid range
    Given two runs provisioned concurrently
    When their user namespaces are inspected from the host
    Then their uid ranges do not overlap

  @live
  Scenario: The invocation is entirely on the command line
    When a run provisions its machine
    Then the nspawn command carries "--settings=false"
    And the ledger records the full command line for the run

  @live
  Scenario: Destroying a machine discards its root
    Given a run whose session created a file at /root/marker inside the machine
    When the run's machine unit is stopped
    Then the ephemeral snapshot is deleted
    And no image subvolume contains /root/marker
```

## Feature: Exec

```gherkin
Feature: Exec
  Every process a step runs, including checks mill runs itself, is a transient unit inside the machine.

  @live
  Scenario: Stdout, stderr and the exit code come back separately and intact
    When mill execs `sh -c 'echo out; echo err >&2; exit 42'` in the machine
    Then stdout is "out"
    And stderr is "err"
    And the exit code is 42

  @live
  Scenario: Output is streamed, not buffered until exit
    When mill execs a process that prints a line, sleeps five seconds, and prints another
    Then the first line is delivered before the second is printed

  @live
  Scenario: Working directory, user and environment are those of the exec
    When mill execs `sh -c 'pwd; id -u; echo $MARK'` with working directory /work, user agent, and MARK=1
    Then stdout is "/work", "1000", "1" on successive lines

  @live
  Scenario: An exec's environment is its own
    Given an exec ran with MARK=1
    When mill execs `sh -c 'echo ${MARK:-unset}'` with no MARK
    Then stdout is "unset"

  @unit
  Scenario: An exec without a step slice is refused
    When the backend is asked to exec with no slice
    Then it returns an error before invoking anything
    And the error says a slice is required

  @live
  Scenario: Cancellation stops the unit, not the client
    Given an exec of `sleep 300` is in flight
    When mill cancels the exec
    Then "mill-exec-<uuid>.service" is inactive inside the machine
    And no sleep process remains in the machine

  @live
  Scenario: A unit that fails to start is retried once and then fails the step
    Given the machine's systemd refuses to start units
    When mill execs a command
    Then the exec is attempted twice
    And the step fails with systemd-run's stderr as evidence
```

## Feature: Step boundary

```gherkin
Feature: Step boundary
  Nothing a step spawned outlives the step.

  @live
  Scenario: A background process does not survive its step
    Given step 1 execs `sh -c 'nohup sleep 3600 &'`
    When step 1 ends
    Then no sleep process exists in the machine
    And step 2 cannot observe one

  @live
  Scenario: A container started by a step does not survive its step
    Given step 1 execs `podman run -d --name lingering quay.io/podman/hello sleep 3600`
    When step 1 ends
    Then `podman ps` inside the machine lists no container named "lingering"

  @live
  Scenario: The next slice waits for the previous one to be inactive
    Given step 1 has a process ignoring SIGTERM
    When step 1 ends
    Then "mill-step-2.slice" is not created until "mill-step-1.slice" reports inactive

  @live
  Scenario: A step that closes on evidence leaves a promotion candidate
    Given step 1 uses the repo-scoped lineage "go-build"
    When step 1 closes on evidence
    Then a read-only snapshot of the run's "go-build" copy exists as the candidate
    And it was taken before the slice was stopped

  @live
  Scenario: A step that is cancelled leaves no candidate
    Given step 1 uses the repo-scoped lineage "go-build"
    When step 1 is cancelled
    Then no candidate snapshot exists for step 1

  @live
  Scenario: A later evidence-closed step replaces the candidate
    Given step 1 closed on evidence with a candidate
    When step 2 closes on evidence
    Then the candidate is step 2's snapshot
    And step 1's snapshot is deleted
```

## Feature: Workspace

```gherkin
Feature: Workspace

  @live
  Scenario: The workspace is the same tree on both sides
    Given a session commits inside /work
    When the host reads the run's workspace directory
    Then `git rev-parse HEAD` agrees on both sides

  @live
  Scenario: Files the session creates are owned by uid 1000 on the host
    Given a session creates /work/file as agent
    When the host stats the file
    Then its owner is 1000

  @live
  Scenario: Commit identity comes from the workspace
    When a session commits with no user.name or user.email in its environment
    Then the commit's author is the workspace's configured identity
    And the commit is signed with the workspace's key

  @live
  Scenario: No host path but the declared binds is visible
    When a session lists mounts inside the machine
    Then the only host-backed mounts are /work, /run/mill, the skill directory, and the declared cache paths
    And /dev/kvm is absent
    And no podman socket from the host is present
```

## Feature: Base image

```gherkin
Feature: Base image

  @live
  Scenario: The base digest is resolved at each provision
    Given Harbor's "mill/base:latest" points at digest A
    And a run has been provisioned from A
    When "mill/base:latest" is retagged to digest B
    And a new run is provisioned
    Then the new run's derived digest is computed from B
    And no worker restart occurred

  @unit
  Scenario: A pinned base is not re-resolved
    Given worker config pins the base to digest A
    When a run is provisioned
    Then the derived digest is computed from A regardless of "latest"

  @unit
  Scenario: An unresolvable base falls back to the last resolved digest
    Given the last resolved base digest is A and its subvolume exists
    And Harbor is unreachable
    When a run is provisioned
    Then the run uses A
    And the run's record notes the fallback

  @unit
  Scenario: An unresolvable base with no fallback fails before boot
    Given no base digest has ever been resolved
    And Harbor is unreachable
    When a run is provisioned
    Then the run fails before any machine is started
    And the evidence is the resolution error

  @live
  Scenario: The base contains the session user, the shim and the toolchain, and nothing of the operator's
    When the base image is inspected
    Then user "agent" exists with uid 1000 and home /home/agent
    Then /usr/local/bin/dnf is the shim and sudoers permits agent only "/usr/bin/dnf install"
    And mise, podman, git, gcc, make and pkgconf are present
    And no file under /root or /home contains a credential
    And the mill skill is not present in the image
```

## Feature: Derived images

```gherkin
Feature: Derived images
  Base plus the packages a repo declares, built on first use, identified by digest.

  @unit
  Scenario: The derived digest is a function of base and sorted packages
    Given base digest A
    When mill.toml declares packages ["vips-devel", "libpq-devel"]
    And another mill.toml declares ["libpq-devel", "vips-devel", "libpq-devel"]
    Then both derive the same digest

  @unit
  Scenario: A repo with no packages derives the base digest
    Given base digest A
    When mill.toml declares no packages
    Then the derived digest is A

  @live
  Scenario: A missing derived image is built and exported before boot
    Given no subvolume exists for the derived digest
    When a run is provisioned
    Then "podman build" runs on the host with "FROM <harbor>/mill/base@A" and the declared packages
    And the result is exported to "mill-image-<digest>.building"
    And renamed to "mill-image-<digest>"
    And the machine boots from it with the packages installed

  @live
  Scenario: Concurrent runs of one repo build the image once
    Given no subvolume exists for the derived digest
    When two runs of the repo are provisioned at once
    Then the second waits on the digest's lock
    And after the first completes the second finds the subvolume and does not build

  @live
  Scenario: A package that does not exist degrades to base with a finding
    Given mill.toml declares package "libfooo-devel"
    When a run is provisioned
    Then the build fails
    And no ".building" subvolume remains
    And the machine boots from the base subvolume
    And the first session receives a finding naming "libfooo-devel" and the last lines of dnf's error

  @live
  Scenario: A build that exceeds its budget degrades to base with a finding
    Given the build time budget is 1 second
    When a run is provisioned with a derived image that takes longer
    Then the build is stopped
    And the run boots from base with a finding naming the budget

  @unit
  Scenario: The build is visibly the worker running a container outside a machine
    When the derived-image build is invoked
    Then it goes through the workspace chokepoint
    And the code path is distinct from exec-in-machine and named as a host build

  @live
  Scenario: Derived images are not pushed to Harbor
    When a derived image is built
    Then Harbor holds no new repository for it
```

## Feature: mill.toml

```gherkin
Feature: mill.toml
  The repo declares what; the worker owns policy.

  @unit
  Scenario: An absent mill.toml means defaults
    Given a checkout with no mill.toml
    When the run's configuration is read
    Then the package list is empty
    And the cache lineages are the ecosystem defaults for the tools in mise.toml

  @unit
  Scenario: Ecosystem defaults follow mise.toml
    Given mise.toml declares tools go and node
    When the run's configuration is read
    Then lineages "mise", "go-mod", "go-build", "npm-cache" and "node_modules" are declared with their default paths and scopes
    And no rust or ruby lineage is declared

  @unit
  Scenario: A declared cache overrides a default of the same name
    Given mise.toml declares go
    And mill.toml declares [cache.go-build] with path "/work/.gocache"
    When the run's configuration is read
    Then lineage "go-build" has path "/work/.gocache"

  @unit
  Scenario: A default can be disabled
    Given mise.toml declares go
    And mill.toml declares [cache.go-mod] with enabled = false
    When the run's configuration is read
    Then no lineage "go-mod" is declared

  @unit
  Scenario Outline: A cache path outside the session's home or /work is a validation error
    Given mill.toml declares [cache.x] with path "<path>"
    When the run's configuration is read
    Then validation fails naming "<path>"

    Examples:
      | path        |
      | /etc        |
      | /var/lib    |
      | /work       |
      | /home/agent |
      | /run/mill   |
      | ../outside  |

  @unit
  Scenario: A validation error does not fail the run
    Given mill.toml is not valid TOML
    When the run's configuration is read
    Then the run proceeds with defaults
    And the first session receives a finding with the parse error

  @unit
  Scenario: Nothing in mill.toml can mount a host path
    Given mill.toml declares [cache.x] with path "/work/x" and any additional keys
    When the provisioning command is generated
    Then every bind's host side is under the run directory, the worker's skill directory, or /run/mill
```

## Feature: Caching

```gherkin
Feature: Caching
  A cache is a lineage of snapshots, restored by name and promoted on evidence.

  @live
  Scenario: A lineage with no generations restores empty
    Given no lineage "go-build" exists for the repo
    When a run restores its caches
    Then /home/agent/.cache/go-build in the machine is an empty writable subvolume

  @live
  Scenario: The latest generation restores
    Given lineage "go-build" has generations 1 and 2, and generation 2 contains file "marker"
    When a run restores its caches
    Then /home/agent/.cache/go-build/marker exists in the machine
    And the run's copy is a writable snapshot of generation 2

  @live
  Scenario: Restore time does not scale with cache size
    Given a lineage whose latest generation is several gigabytes
    When a run restores it
    Then the restore completes in under one second

  @live
  Scenario: A run's writes do not touch the generation it restored from
    Given a run restored generation 2
    When the session writes to the cache path
    Then generation 2 is unchanged

  @live
  Scenario: Promotion comes from the last evidence-closed step
    Given step 1 closed on evidence after writing "a" to the cache
    And step 2 wrote "b" and was cancelled
    When the run finishes
    Then the new generation contains "a" and not "b"

  @live
  Scenario: A run with no evidence-closed step promotes nothing
    Given every step of a run failed or was cancelled
    When the run finishes
    Then the lineage's generation count is unchanged

  @live
  Scenario: A run that fails at review still promotes the build cache
    Given the build step closed on evidence
    And the review step returned revise until the retry cap
    When the run finishes
    Then a new generation exists from the build step's snapshot

  @live
  Scenario: A lineage keeps its two newest generations
    Given lineage "go-build" has generations 1 and 2
    When a run promotes generation 3
    Then generations 2 and 3 exist
    And generation 1 has been deleted

  @live
  Scenario: Global lineages are shared across repos
    Given a run of repo A promoted the global lineage "go-mod" with module "example.com/m@v1"
    When a run of repo B restores its caches
    Then module "example.com/m@v1" is present in its go-mod copy

  @live
  Scenario: Repo-scoped lineages are not shared across repos
    Given a run of repo A promoted the repo-scoped lineage "go-build"
    When a run of repo B restores its caches
    Then its "go-build" copy is empty

  @live
  Scenario: A global lineage over its cap is reset, not trimmed
    Given the global lineage "cargo-registry" has a cap of 1 GB
    And a run's copy has grown to 2 GB
    When the run finishes
    Then no generation is promoted
    And the lineage has no generations
    And the next run restores it empty

  @live
  Scenario: Concurrent promotions to one lineage both succeed and the later wins
    Given two runs of one repo restored generation 1
    When both finish and promote
    Then the lineage has two new generations
    And the latest is from the run that promoted last

  @unit
  Scenario: An optional key invalidates a lineage when its files change
    Given mill.toml declares [cache.target] with key ["rust-toolchain.toml"]
    And the latest generation recorded key hash H
    When the checkout's rust-toolchain.toml hashes to H'
    Then the run restores "target" empty
    And the promoted generation records H'

  @unit
  Scenario: The manifest is the worker's, and the ledger records only use
    When a run restores and promotes lineage "go-build"
    Then the lineage's manifest.json records the generation's created-at, size estimate, producing run and step
    And the ledger records that the run restored and promoted "go-build" and nothing else about it
```

## Feature: Eviction

```gherkin
Feature: Eviction
  Expiry, then least-recently-used under free-space watermarks; globals bounded by cap.

  @live
  Scenario: An unused repo-scoped lineage expires
    Given lineage "go-build" for repo A was last restored 31 days ago
    And the expiry is 30 days
    When eviction runs
    Then the lineage is deleted

  @live
  Scenario: An unused image subvolume expires
    Given "mill-image-<digest>" was last booted 31 days ago
    When eviction runs
    Then the subvolume is deleted

  @live
  Scenario: A recently used lineage does not expire
    Given lineage "go-build" for repo A was restored yesterday
    When eviction runs
    Then the lineage remains

  @live
  Scenario: Below the high watermark, least recently used lineages are evicted until above the low
    Given free space is below the high watermark
    And repo-scoped lineages for repos A, B and C last used 10, 5 and 1 days ago
    When eviction runs
    Then A's lineages are deleted first
    And eviction stops as soon as free space exceeds the low watermark

  @live
  Scenario: Eviction is decided by free space, not by summed estimates
    Given the manifests' size estimates sum above the high watermark
    But statfs reports free space above it
    When eviction runs
    Then nothing is evicted

  @live
  Scenario: Eviction waits for space to be freed before re-checking
    Given free space is below the high watermark
    When eviction deletes a subvolume
    Then it runs "btrfs subvolume sync" before reading free space again

  @live
  Scenario: Global lineages are not evicted by watermark
    Given free space is below the high watermark
    And the only lineages are global
    When eviction runs
    Then no global lineage is deleted

  @live
  Scenario: Evicting a lineage does not affect a run that restored from it
    Given a run restored generation 2 of lineage "target"
    When eviction deletes lineage "target"
    Then the run's copy is intact
    And the run's builds continue to use it

  @live
  Scenario: A run whose lineage was evicted promotes into a fresh lineage
    Given a run restored generation 2 of lineage "target"
    And eviction deleted the lineage during the run
    When the run finishes
    Then lineage "target" exists with a single generation 1 from this run

  @unit
  Scenario: A per-lineage cap on repo scope is flagged, not enforced
    Given repo lineage "target" has a cap of 5 GB and a latest generation of 8 GB
    When "mill cache ls" runs
    Then the lineage is listed with an over-cap flag
    And a subsequent run still restores and promotes it

  @unit
  Scenario: Eviction runs at each trigger
    Then eviction runs after each promotion
    And at worker start after reconciliation
    And on the worker's timer for expiry
```

## Feature: Network

```gherkin
Feature: Network
  Egress is enforced on the host, outside the boundary being policed.

  @live
  Scenario: The outside world is reachable through the proxy
    When a session runs `curl https://example.com/` with the exec's proxy environment
    Then the response status is 200

  @live
  Scenario: The proxy does not intercept
    When a session runs `curl -v https://example.com/` with the exec's proxy environment
    Then the certificate presented is the origin's

  @live
  Scenario: Direct egress is dropped
    When a session runs `curl --max-time 5 https://example.com/` with no proxy environment
    Then the connection times out

  @live
  Scenario: Machines cannot reach each other
    Given two runs' machines are up
    When one attempts a TCP connection to the other's veth address
    Then the connection is dropped

  @live
  Scenario: Machines cannot reach the host's other services
    Given the host listens on a port other than the proxy
    When a session attempts to connect to it
    Then the connection is dropped

  @live
  Scenario: Container images come through Harbor's proxy cache
    When a session runs `podman pull docker.io/library/postgres:16`
    Then the pull is served by the Harbor proxy project for docker.io
    And the proxy's access log shows no connection to docker.io

  @live
  Scenario: A container's published port is reachable on the machine's loopback and not from the host
    Given a session runs `podman run -d -p 5432:5432 docker.io/library/postgres:16`
    Then a connection to 127.0.0.1:5432 inside the machine succeeds
    And a connection to the machine's veth address port 5432 from the host is refused

  @live
  Scenario: The proxy environment is set on every exec
    When mill execs `env` in the machine
    Then HTTPS_PROXY, HTTP_PROXY and NO_PROXY are present

  @unit
  Scenario: The session is told that egress goes through the proxy
    When the session's system prompt and the mounted skill are read
    Then both state that all outbound connections must use the proxy named in the environment

  @unit
  Scenario: The proxy variables cross the harness adapter's floor
    Given --env-mode strict
    When the adapter composes a sidecar's environment for a step whose workspace set HTTPS_PROXY
    Then HTTPS_PROXY, HTTP_PROXY and NO_PROXY are present in the sidecar's environment
    And no other variable outside the floor is

  @live
  Scenario: The proxy variables reach a session's shell
    When a session runs `env` through its Bash tool
    Then HTTPS_PROXY, HTTP_PROXY and NO_PROXY are present
```

## Feature: Nested machines

```gherkin
Feature: Nested machines
  A repo whose tests boot machines may do so inside its own machine.

  @unit
  Scenario: nested is a request the worker grants
    Given mill.toml declares [machine] nested = true
    And worker config does not grant nested for this repo
    When the run is provisioned
    Then the machine is provisioned without nested
    And the first session receives a finding saying nested was requested and not granted

  @unit
  Scenario: A granted nested request changes three things
    Given nested is granted for this repo
    When the provisioning command is generated
    Then the uid mapping is a range of 1048576, not pick
    And the host's image directory is bound read-only at /var/lib/machines
    And the session user has sudo
    And nothing else in the command differs from an ungranted provision

  @live
  Scenario: A nested machine boots an inner machine and execs into it
    Given a machine provisioned with nested
    When a session runs `sudo systemd-run -M inner --pipe --wait -- sh -c 'exit 7'` after booting "inner" from a bound image
    Then the exit code is 7
    And machinectl inside the outer lists "inner"

  @live
  Scenario: Podman runs inside the inner machine
    Given an inner machine is running
    When a session runs `podman run --rm quay.io/podman/hello` inside it
    Then the exit code is 0

  @live
  Scenario: Subvolumes can be created, snapshotted and deleted from inside a nested machine
    Given a machine provisioned with nested on a host mounted with user_subvol_rm_allowed
    When a session creates, snapshots and deletes a subvolume as agent
    Then all three succeed

  @live
  Scenario: Inner machines die with the outer
    Given an outer machine with two inner machines running
    When the outer's machine unit is stopped
    Then no machine named "inner-*" is registered anywhere

  @live
  Scenario: A nested run does not promote into the outer's lineages
    Given an inner run whose step closed on evidence after writing to a cache path
    When the inner run finishes
    Then the outer repo's lineage has no new generation
    And the inner's generation lives under a lineage root inside the outer's ephemeral root

  @unit
  Scenario: A promotion that addresses an outer lineage from a nested run is refused
    Given a nested worker
    When it attempts to promote into a lineage path outside its own root
    Then the promotion is refused and recorded
```

## Feature: Budgets

```gherkin
Feature: Budgets
  Steps run under the workflow's timeout, and a session can wait in the foreground.

  @unit
  Scenario: Every preparatory step has an explicit timeout
    Given a workflow whose toolchain step declares no timeout
    When the workflow is loaded
    Then loading fails naming the step and the missing timeout

  @unit
  Scenario: The sidecar sets the session's shell timeout to the step's budget
    Given a step with timeout 20m
    When the sidecar starts the session
    Then the session's default shell timeout is 20m
    And a command that runs 8 minutes in the foreground is not backgrounded
```

## Feature: External state

```gherkin
Feature: External state
  A machine may only create external state that is swept by name from the host side.

  @live
  Scenario: The live suite refuses the deployed dagger
    Given MILL_ACCEPTANCE_DAGGER_SOCKET names /run/dagger/dg.sock
    When the live suite starts
    Then it refuses before booting anything
    And the refusal names the socket and the testsupport dagger fixture to use instead

  @live
  Scenario: The live suite files into a throwaway dagger
    When the live suite starts
    Then a `dg serve` from internal/testsupport is listening on a temporary socket
    And every node the suite files is in a project on that socket

  @db
  Scenario: The acceptance suite creates nothing in the deployed dagger by default
    Given MILL_ACCEPTANCE_DAGGER_SOCKET is unset
    When make test-acceptance runs
    Then `dg project list` on the deployed dagger is unchanged before and after

  @db
  Scenario: Fencing and resume pass against the throwaway dagger
    Given the suite's dagger is the testsupport instance
    When the fencing and resume scenarios run
    Then both pass
    And the second claim was a second client under the same principal

  @unit
  Scenario: The deployed-dagger flag is honoured on the local workspace and refused in a machine
    Given MILL_ACCEPTANCE_DAGGER_SOCKET names the deployed dagger
    When the suite runs on the local workspace
    Then it uses that socket and the runbook says so
    When the live tier runs inside a machine
    Then it refuses before booting anything

  @live
  Scenario: The fixture's prerequisites are present in the mill repo's machine
    Given the mill repo's machine
    When a session checks for `dg`, `goose`, and the dagger migrations
    Then all three are present, from the read-only dagger checkout bind or the published image

  @live
  Scenario: A suite's machines are swept after a kill
    Given a live suite booted "mill-acceptance-x-<nonce>" and was SIGKILLed
    When the worker reconciles
    Then the machine is stopped and its run directory removed

  @db
  Scenario: Stale scratch dagger projects are archived by name
    Given a project "mill-acceptance-x-<nonce>" older than the suite's budget with no live run
    When the worker reconciles
    Then the project is archived

  @db
  Scenario: Stale scratch forge projects are deleted by name
    Given a forge project "mill-acceptance-x-<nonce>" in the scratch namespace older than the suite's budget
    When the worker reconciles
    Then the project is deleted

  @unit
  Scenario: A suite never derives a workflow at org scope
    When a scenario derives a workflow definition
    Then it is pushed at the scenario's project scope
    And an attempt to push at org scope from a suite fails the scenario

  @unit
  Scenario: A suite's external state is prefix-named
    When a scenario creates a machine, a dagger project, or a forge project
    Then each name begins with "mill-acceptance-" and ends with the scenario's nonce
```

## Feature: Self-hosting

```gherkin
Feature: Self-hosting
  The sandbox is built on the local workspace, gated once, then tests itself.

  @unit
  Scenario: A live spec skips with a message naming its cost
    Given MILL_SANDBOX_HOST is unset
    When a live spec runs under make test
    Then it skips
    And the skip message says to set MILL_SANDBOX_HOST=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine

  @unit
  Scenario: The bootstrap gate's criterion is the suite's output
    Given the node that creates the backend
    When its milestone is inspected
    Then exactly one child has assignee_kind user
    And its done-when is the ginkgo summary of MILL_SANDBOX_HOST=1 make test on the host, completed as the gate's result

  @unit
  Scenario: The workspace backend is chosen by configuration
    Given worker config names the machine backend
    When the run loop constructs its Workspaces function
    Then it is the machine backend
    And with no configuration it is the local backend
```

## Feature: Credentials

```gherkin
Feature: Credentials
  A session never sees a credential it could keep.

  @live
  Scenario: The step token is not in the machine
    Given a step with an issued token
    When a session runs `env` and greps every readable file under /home/agent, /work and /run for the token
    Then nothing matches

  @live
  Scenario: A git credential is minted over the socket and dies with the step
    Given step 1's session runs `git push`
    Then the credential helper obtains a credential over /run/mill
    And the credential is minted against step 1's token
    When step 1 ends
    Then the credential is revoked
    And a push with it in step 2 is refused by the forge

  @live
  Scenario: No token is written under /work or /home/agent
    Given a session performed a push
    When /work and /home/agent are searched for the minted credential
    Then nothing matches

  @live
  Scenario: There is no login in the machine
    When a session lists /home/agent/.claude
    Then no .credentials.json exists
    And `claude` still answers a prompt

  @live
  Scenario: The session's model requests go to the gateway with the step token
    When a session runs `claude -p 'reply ok'`
    Then the gateway's log shows a request whose bearer is the step token
    And the request forwarded upstream carries the subscription credential, not the step token
    And the session's environment contains no credential other than the step token

  @live
  Scenario: A rotated step token is refused by the gateway
    Given step 1's token was in use
    When step 1 ends and Rotate installs step 2's token
    And a request with step 1's token reaches the gateway
    Then the gateway refuses it

  @unit
  Scenario: The gateway records usage per step from outside the session
    When a response passes through the gateway for step 3 with model M
    Then the ledger holds a usage row for (step 3, M) attributed to the gateway
    And it is distinct from the SDK-reported row for the same turn

  @unit
  Scenario: A step over its cost budget is refused at the gateway
    Given step 3 has a token budget and has consumed it
    When the session makes another model request
    Then the gateway refuses it
    And the step fails with the budget as evidence

  @unit
  Scenario: mill doctor warns before the setup-token expires
    Given the setup-token was minted eleven months ago
    When `mill doctor` runs
    Then it warns that the model credential expires within a month
```

## Feature: Storage

```gherkin
Feature: Storage
  The sandbox lives on a btrfs volume of its own.

  @live
  Scenario: The sandbox volume is btrfs with subvolume deletion allowed to owners
    When the filesystem under /var/lib/mill is inspected
    Then it is btrfs
    And its mount options include user_subvol_rm_allowed
    And /var/lib/machines is a subvolume of it

  @unit
  Scenario: The worker refuses to start on a non-btrfs sandbox root
    Given /var/lib/mill is on XFS
    When the worker starts
    Then it refuses
    And the refusal names the filesystem and the mill_storage role
```

## Feature: Session identity

```gherkin
Feature: Session identity

  @live
  Scenario: Sessions run as agent, uid 1000, not root
    When mill execs `id -u; id -un; test -w /etc/passwd && echo writable || echo readonly`
    Then stdout is "1000", "agent", "readonly"

  @live
  Scenario: The home is ephemeral except for cache lineages
    Given a session writes /home/agent/scratch and /home/agent/.cache/go-build/kept
    And "go-build" is a repo-scoped lineage
    And the step closes on evidence
    When the next run of the repo starts
    Then /home/agent/scratch does not exist
    And /home/agent/.cache/go-build/kept exists

  @live
  Scenario: The harness's own scratch under ~/.claude is lost with the run
    Given a session writes /home/agent/.claude/history.jsonl
    When the next run of the repo starts
    Then the file does not exist

  @live
  Scenario: Only the dnf shim escalates
    When a session runs `sudo -l`
    Then the only permitted command is "/usr/bin/dnf install"
```

## Feature: Skill mount

```gherkin
Feature: Skill mount
  The skill and the sidecar are the worker's, versioned with the binary.

  @live
  Scenario: The skill is mounted read-only at the harness's path
    When a session lists /home/agent/.claude/skills/mill
    Then the mill skill is present
    And an attempt to write there fails with a read-only filesystem error

  @live
  Scenario: The mounted skill matches the worker's version
    Given the worker is at version V
    When a session reads the skill's version breadcrumb
    Then it is V

  @live
  Scenario: The rest of ~/.claude is writable
    When a session writes /home/agent/.claude/settings.local.json
    Then the write succeeds

  @unit
  Scenario: The skill documents mill.toml
    When the skill's body is read
    Then it describes [image].packages, [cache.<name>].path, the path rules, the ecosystem defaults, and that runtime dnf installs are not remembered
```

## Feature: The dnf shim

```gherkin
Feature: The dnf shim

  @live
  Scenario: A session can install a package into the ephemeral root
    When a session runs `dnf install -y vips-devel`
    Then the exit code is 0
    And `rpm -q vips-devel` succeeds in the machine

  @live
  Scenario: The hint is the last lines of the output
    When a session runs `dnf install -y vips-devel`
    Then the final lines of stdout name "vips-devel", say the machine is ephemeral, and show the [image].packages line to add to mill.toml

  @live
  Scenario: The install is recorded in the run's provenance, not sent to the reviewer
    When a session runs `dnf install -y vips-devel`
    Then the run's provenance records "installed vips-devel, not declared in mill.toml"
    And the reviewer's input does not mention it

  @live
  Scenario: Only install is permitted
    When a session runs `sudo /usr/bin/dnf remove -y vips-devel`
    Then sudoers refuses

  @live
  Scenario: A runtime install is not remembered
    Given a session installed "vips-devel" and did not change mill.toml
    When the next run of the repo starts
    Then `rpm -q vips-devel` fails in the machine

  @live
  Scenario: A declared package is remembered
    Given a session installed "vips-devel" and committed it to [image].packages
    When the next run of the repo starts
    Then `rpm -q vips-devel` succeeds in the machine before any session runs
```

## Feature: Preparatory steps

```gherkin
Feature: Preparatory steps
  A preparatory step readies the machine; its failure is a finding, never a failed run.

  @live
  Scenario: The toolchain step trusts the project and installs its tools
    Given mise.toml declares go
    When the toolchain step runs
    Then `mise trust` has accepted /work
    And `mise which go` succeeds in the machine
    And the step's evidence is the exit code and captured output

  @live
  Scenario: The toolchain step's output is not in the session's context
    When the toolchain step runs and the first session starts
    Then the session's transcript does not contain mise's install output

  @live
  Scenario: A failed toolchain step becomes a finding and the run continues
    Given mise.toml declares a tool version that does not exist
    When the toolchain step runs
    Then it exits non-zero
    And the run continues into the first session
    And the session receives a finding with the last lines of the output

  @live
  Scenario: The agent can fix the toolchain and the next run passes
    Given a run whose toolchain step failed
    And the session corrected mise.toml and committed
    When the next run's toolchain step runs
    Then it exits zero

  @unit
  Scenario: A preparatory step has a time budget
    Given the toolchain step's budget is 1 second
    When the step exceeds it
    Then the step is stopped
    And the finding names the budget

  @unit
  Scenario: No preparatory step can trip the circuit breaker
    Given every preparatory step of a run failed
    When the run's retry accounting is inspected
    Then no attempt has been consumed
```

## Feature: Podman inside the machine

```gherkin
Feature: Podman inside the machine

  @live
  Scenario: Podman uses native overlay and the systemd cgroup manager
    When a session runs `podman info`
    Then the graph driver is "overlay"
    And the cgroup manager is "systemd"

  @live
  Scenario: A container runs as agent without fuse
    When a session runs `podman run --rm quay.io/podman/hello`
    Then the exit code is 0
    And /dev/fuse was not exposed to the machine

  @live
  Scenario: The db-labelled specs pass inside a machine
    Given /work is a checkout of mill
    And the toolchain step passed
    When a session runs `make test-db` with DOCKER_HOST pointing at the machine's podman socket
    Then the exit code is 0
    And no container remains after the step ends
```

## Feature: Reconciliation on resume

```gherkin
Feature: Reconciliation on resume

  @db @live
  Scenario: An orphan machine is stopped and its run directory removed
    Given a machine "mill-<id>" is registered
    And no live run with that id exists in the ledger
    When the worker starts
    Then the machine unit is stopped
    And the run directory is deleted
    And the reconciliation is recorded

  @db @live
  Scenario: A live run with no machine fails as a lost sandbox
    Given a live run in the ledger at step 2
    And no machine "mill-<id>" is registered
    When the worker starts
    Then the run fails with "lost sandbox" as evidence
    And no new machine is provisioned for it

  @db @live
  Scenario: A live run with its machine resumes at its step with a fresh slice
    Given a live run in the ledger at step 2 with a process still running in "mill-step-2.slice"
    When the worker starts
    Then "mill-step-2.slice" is stopped and recreated
    And the run resumes at step 2

  @db @live
  Scenario: A candidate from before the crash still promotes
    Given a live run whose step 1 closed on evidence with a candidate
    And the worker died during step 2
    When the worker resumes and the run finishes
    Then the candidate from step 1 is promoted

  @live
  Scenario: Leftover builds are removed
    Given "/var/lib/machines/mill-image-<digest>.building" exists
    When the worker starts
    Then it is deleted

  @live
  Scenario: Run directories with no live run are removed
    Given "/var/lib/mill/runs/<id>" exists with cache subvolumes
    And no live run with that id exists
    When the worker starts
    Then the subvolumes are deleted and the directory removed

  @unit
  Scenario: Reconciliation precedes eviction
    When the worker starts
    Then orphans and leftovers are handled before eviction runs
```

## Feature: Privilege

```gherkin
Feature: Privilege

  @live
  Scenario: The worker is not root and the payload is not either
    When the worker provisions a machine and execs `id -u` in it
    Then the worker process runs as the operator's uid on the host
    And the machine's unit is "mill-machine@<run-id>.service" on the system manager, owned by root
    And the exec reports uid 1000 inside
    And the host sees the session's processes under an unprivileged uid in the machine's range

  @live
  Scenario: The polkit rule admits the template and nothing else
    Given a shell as the operator without a tty
    When `systemctl start mill-machine@x.service` is run
    Then it starts without a password prompt
    When `systemctl start any-other.service` is run the same way
    Then it is refused

  @live
  Scenario: The operator can exec into a machine without root
    Given a machine started through the template
    When the operator runs `systemd-run -M <machine> --pipe --wait -- sh -c 'exit 42'` with no sudo
    Then the exit code is 42

  @live
  Scenario: A session cannot reach the host's filesystem
    When a session runs `ls /var/lib/mill` and `cat /etc/shadow`
    Then both fail
    And the machine's /etc/shadow is the image's, not the host's
```

## Feature: Operator tools

```gherkin
Feature: Operator tools

  @unit
  Scenario: mill cache ls lists lineages with scope, generations, size estimate and last use
    Given lineages exist in both scopes
    When "mill cache ls" runs
    Then each lineage is listed with its scope, generation count, latest size estimate, last-restored time, and any over-cap flag

  @unit
  Scenario: mill cache prune runs eviction and reports what it deleted
    Given an expired lineage exists
    When "mill cache prune" runs
    Then the lineage is deleted
    And the output names it and the reason

  @unit
  Scenario: mill image ls lists image subvolumes with digest, base, packages and last boot
    When "mill image ls" runs
    Then each subvolume is listed with its derived digest, the base digest it was built from, the package list, and last-booted time
```

## Feature: Failure modes

```gherkin
Feature: Failure modes

  @live
  Scenario: A machine that dies mid-step fails the run as a lost sandbox and keeps earlier candidates
    Given step 1 closed on evidence with a candidate
    And step 2 is in flight
    When the machine unit is killed from the host
    Then step 2's exec returns a connection error
    And the run fails with "lost sandbox"
    And step 1's candidate is promoted

  @live
  Scenario: A step over budget is stopped without a candidate
    Given step 1's budget is 1 second
    When step 1 runs longer
    Then the slice is stopped
    And the step fails
    And no candidate exists for it

  @live
  Scenario: A full cache filesystem is handled by eviction, then by finishing without promotion
    Given the cache filesystem has no free space
    When a run finishes and promotion fails with ENOSPC
    Then eviction runs and promotion is retried once
    And if it fails again the run finishes with a note that nothing was promoted

  @live
  Scenario: A run directory that cannot be removed does not block finishing
    Given the run directory's removal fails
    When the run finishes
    Then the run is marked finished
    And the failure is logged for the next reconciliation
```
