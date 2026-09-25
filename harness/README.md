# Harness

Works one epic in a running skift service. One node per iteration, two agents.

```
harness/.venv/bin/python harness/loop.py --epic '#1' --dry-run   what is ready under the epic
harness/.venv/bin/python harness/loop.py --epic '#1'             run until the epic drains
harness/.venv/bin/python harness/loop.py --epic '#1' --max 1     one node
harness/.venv/bin/python harness/loop.py --report                per-node tokens and cost
```

The project comes from `--project`, then `$SKIFT_PROJECT`, then `skift`. The
service is reached the way `skift` reaches it: the socket, or `$SKIFT_ADDRESS`.

## Scope

The epic is a grouping node. On start, every open child gets the label
`epic-<N>`, and the harness claims only nodes carrying it. That is the whole
scoping mechanism, so it is legible in the graph: `skift node list` shows what is
in scope, and removing the label takes a node out.

## How a node is worked

1. `skift claim -l epic-N --assignee-kind agent` takes the most urgent ready child
   under a one-hour lease. The harness renews the lease every ten minutes while
   an agent runs, and releases it with reason `crash` if the harness dies.
2. The work order is the node's body plus **inputs**: the result of every done
   node that blocks it. That is the handoff. A node completed with a result of
   "done" hands nothing on.
3. The **implementer** receives the work order. It may not touch `harness/`,
   `scripts/`, `.claude/`, or `backlog.toml`; violations are reverted and fed
   back as a review failure. Documents under `docs/` and `plugin/` are not
   protected, because work orders name them as deliverables.
4. The **reviewer** receives the same work order and the working tree. It
   decides one thing: whether the acceptance criteria and Gherkin scenarios are
   satisfied. It cannot write files, so it cannot fix what it should fail.
5. `VERDICT: PASS` commits, pushes, and completes the node. The result is the
   implementer's own summary, the commit, and the files changed.
   `VERDICT: FAIL` returns the unmet criteria to the implementer, up to
   `--attempts` times.
6. A node that exhausts its attempts gets the last review as a comment, trades
   `epic-N` for `harness-blocked` so the claim filter stops returning it, and
   its lease is released with reason `budget`. Put the scope label back to
   retry it.

A child that has children of its own and no acceptance criteria is a grouping
node; when skift makes it ready, the harness completes it as bookkeeping.

Gates (`assignee_kind: user`) are never claimed; the dry run lists them as
waiting on a person.

## State

Status, leases, results and comments live in skift. `harness/state.json`
holds only what skift cannot take yet: attempt counts per node and the usage
history behind `--report`. It also keeps the last review of a node that failed, so a
relaunch resumes with it. Delete it to reset attempts; nothing in the graph
changes.

`harness/STOP` is written on exit: `DONE:` when every child is closed,
`WAITING:` when children are open but none is claimable, `BLOCKED:` when nodes
exhausted their attempts or the budget was reached.

## Defaults

Opus 5 at medium effort, three attempts per node, one hour per agent,
permission mode `auto`.

An agent's session outlives its turn while a background command, subagent, or
monitor it started is still running; the CLI wakes it when the task finishes.
The session ends at the first turn that ends with nothing running.

## Watching a run

```
tail -f harness/logs/loop.log
```

Every line the harness and both agents produce, as it happens: the agent's own
text, the intent it states for each command and the command itself, one line
per tool result with its outcome and size, the verdict, the commit, and the
completion. Full tool output is never logged. The same lines land in
`harness/logs/<number>-<title>.log`, one file per node.
