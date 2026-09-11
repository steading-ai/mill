#!/usr/bin/env python3
"""harness/loop.py — work an epic in a running dagger service.

One node per iteration. The harness claims the most urgent ready child of the
epic under a lease, an implementer agent builds it, a reviewer agent decides
whether the node's acceptance criteria and Gherkin scenarios are satisfied. On
a pass the harness commits, pushes, and completes the node with a result the
next node's work order will carry as an input. On a fail the reviewer's
findings go back to the implementer, up to a retry cap.

The reviewer judges the contract only. It is explicitly not a code reviewer.

    harness/.venv/bin/python harness/loop.py --epic '#1'      run until the epic drains
    harness/.venv/bin/python harness/loop.py --epic '#1' --dry-run
    harness/.venv/bin/python harness/loop.py --epic '#1' --max 3
    harness/.venv/bin/python harness/loop.py --report

Exit codes: 0 drained, 1 error or cap reached, 2 blocked.
"""

import argparse
import asyncio
import io
import json
import os
import re
import signal
import socket
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

from claude_agent_sdk import (
    TERMINAL_TASK_STATUSES,
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    HookMatcher,
    ResultMessage,
    TaskNotificationMessage,
    TaskStartedMessage,
    TaskUpdatedMessage,
    TextBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

ROOT = Path(__file__).resolve().parent.parent
STATE = ROOT / "harness" / "state.json"
STOP = ROOT / "harness" / "STOP"
LOGS = ROOT / "harness" / "logs"

SESSION_URL = "https://claude.ai/code/session_017sEP9E8M4gp2qQ7DjYUbKu"

# The implementer must not rewrite the machinery driving it. Design docs are
# not protected: the work orders in dagger name them as deliverables.
PROTECTED = ("backlog.toml", "harness/", "scripts/", ".claude/")

# The harness writes these itself while an agent is running, so they are never
# evidence that the implementer went out of bounds.
HARNESS_RUNTIME = ("harness/state.json", "harness/logs/", "harness/STOP", "harness/.venv/")

# Leases are bounded by the server at one hour. Renew well inside that.
HEARTBEAT_EVERY = 600
LEASE_TTL = "1h"

# A turn that ends with a background task still running keeps the session
# open: the CLI wakes the agent when the task ends. Every HEARTBEAT seconds of
# that wait the harness looks at the task's output file. Growing output is
# work and earns silence. No output earns a heartbeat turn that asks the agent
# whether it is done. HEARTBEAT_STRIKES silent heartbeats trip the breaker:
# the task's processes are killed and the result already in hand is taken.
# Twice a session waited on `until ! pgrep -f "ginkgo"` — a loop whose own
# shell matches the pattern — and the only thing that ended it was the hour.
HEARTBEAT = 180
HEARTBEAT_STRIKES = 3

# The CLI backgrounds a foreground command that runs past this, and a
# backgrounded test run is what taught sessions to poll. Thirty minutes: the
# unit suite takes half a minute warm and a little over one cold, and the
# acceptance suite takes eight, which a ten-minute ceiling left no room over.
# A session that can wait in the foreground never has a task to lose.
BASH_TIMEOUT_MS = "1800000"

HEARTBEAT_PROMPT = """\
You ended your turn {minutes} minutes ago with a background task still running and \
it has produced no output since: {tasks}. Is your work done, or is that task \
still needed? If you are done, stop the task (TaskStop) and say so in one line. \
If it is needed, say what it is doing and wait on it with TaskOutput, not a shell \
loop. This is heartbeat {strike} of {strikes}; after the last one the task is killed \
and your last summary is taken as your result."""

# A node that exhausts its attempts trades the scope label for this one, so
# the claim filter stops returning it. Put the scope label back to retry it.
BLOCKED_LABEL = "harness-blocked"

IMPLEMENTER_PROMPT = """\
You are implementing exactly one node of dagger's own work graph, in a
repository you are seeing for the first time. The node is held under a lease
by the harness that dispatched you; you have no dagger tools and need none.

Before writing code, read the documents the work order references. They are the
authority: docs/domain-model.md, docs/architecture.md, docs/core-principles.md,
and the ADRs under docs/adrs/. The work order tells you which matter. Where a
work order names a document under docs/ or plugin/ as a deliverable, changing
it is the work.

If the work order carries inputs from predecessor nodes, they are the results
of the work this node depends on. Read them before the code; they say what was
built and where.

Your job is to satisfy the node's contract:

- Every acceptance criterion must hold.
- Every Gherkin scenario must exist as an executable spec. Ginkgo and Gomega,
  in the package the code lives in. The scenario text becomes the spec name.
- `make lint` and `make test` must pass when you are done.

Rules:

- Do not modify anything under harness/, scripts/, or .claude/, nor
  backlog.toml. Those are the machinery driving you. If you believe one of
  them is wrong, say so in your final message and continue.
- Do not commit. The harness commits after review.
- Do not start work belonging to another node. If a dependency turns out to be
  missing, say so in your final message rather than building it.
- Prefer the standard library and the pinned toolchain. Add a dependency only
  when the work order names it.

Finish with a summary that a downstream agent can work from: what you built,
which files, what changed on the wire or in the CLI if anything, and anything
the reviewer should look at first. It becomes the node's result.
"""

REVIEWER_PROMPT = """\
You are a contract reviewer. You decide one thing: whether the work satisfies
the acceptance criteria and Gherkin scenarios of the node you are given.

You are NOT a code reviewer. Do not comment on style, naming, formatting, file
layout, abstraction, duplication, performance, or design taste. Do not suggest
improvements. Do not bikeshed. A criterion met by ugly code is met.

How to judge:

- Work through the acceptance criteria one at a time. For each, find the
  evidence: the code that implements it, and the spec that proves it.
- Work through the Gherkin scenarios one at a time. Each must exist as an
  executable spec whose assertions actually correspond to the Then clauses. A
  spec that is named after the scenario but asserts nothing is a fail.
- Run the suite yourself. `make test` and `make lint`. Read the output.
- A criterion you cannot verify is a fail, not a pass with a caveat. A
  criterion that requires a deployed service or a person is out of your reach;
  say so and judge the rest.
- A failure in a suite the diff does not touch gets one isolated rerun. If
  that passes, it is a flake: name it as friction in your response and move
  on. Do not spend further runs establishing it was not the work's doing.

Reasons to fail:

- A criterion or scenario is unimplemented, or implemented only in prose.
- A spec exists but does not assert what the scenario says.
- Tests do not pass, or do not run.
- The work touched harness/, scripts/, .claude/, or backlog.toml.

End your response with exactly one line in this form, and nothing after it:

VERDICT: PASS

or

VERDICT: FAIL

If FAIL, the lines immediately before the verdict must list each unmet
criterion or scenario, one per line, starting with "- ", quoting the criterion
and stating what is missing. Be specific enough that the implementer can fix it
without guessing.
"""


def now():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def say(s):
    """A harness status line: stdout and the run-wide log, flushed at once."""
    print(s, flush=True)
    LOGS.mkdir(parents=True, exist_ok=True)
    with open(LOGS / "loop.log", "a") as f:
        f.write(s + "\n")


# --- dagger ------------------------------------------------------------------


class Dagger:
    """The dg CLI over the current project. Every call is one subprocess."""

    def __init__(self, project):
        self.project = project

    def run(self, *args, stdin=None, check=True):
        r = subprocess.run(
            ["dg", "-p", self.project, "-o", "json", *args],
            cwd=ROOT, capture_output=True, text=True, input=stdin,
        )
        if r.returncode != 0 and check:
            raise RuntimeError(f"dg {' '.join(args)}: {r.stderr.strip() or r.stdout.strip()}")
        return json.loads(r.stdout) if r.stdout.strip() else {}

    def ref(self, node):
        return f"{self.project}#{node['number']}"

    def show(self, ref):
        return self.run("node", "show", ref)

    def list(self, **filters):
        args = []
        for k, v in filters.items():
            args += [f"--{k.replace('_', '-')}", v]
        return self.run("node", "list", *args).get("nodes", [])

    def ready(self, label):
        return self.run("ready", "-l", label, "--assignee-kind", "agent").get("nodes", [])

    def claim(self, label, session):
        r = self.run(
            "claim", "-l", label, "--assignee-kind", "agent", "--ttl", LEASE_TTL,
            "--harness", "harness/loop.py", "--session-ref", session,
            "--agent-ref", socket.gethostname(),
        )
        return (r.get("node"), r.get("lease")) if r.get("node") else (None, None)

    def heartbeat(self, lease_id):
        self.run("heartbeat", lease_id, "--ttl", LEASE_TTL)

    def release(self, lease_id, reason, detail):
        self.run("release", lease_id, "--reason", reason, "--detail", detail, check=False)

    def complete(self, ref, lease_id, result):
        self.run("complete", ref, "--lease", lease_id, "--result", "-", stdin=result)

    def comment(self, ref, lease_id, body):
        self.run("comment", ref, "-", "--lease", lease_id, stdin=body)

    def patch(self, ops):
        return self.run("patch", "apply", "-", stdin=json.dumps({"ops": ops}))

    def set_labels(self, node, labels, lease_id=None):
        op = {"node": self.ref(node), "labels": {"values": sorted(labels)}}
        if lease_id:
            op["lease_id"] = lease_id
        self.patch([{"update_node": op}])


def scope_epic(dg, epic_ref, label):
    """Label every open child of the epic so the claim filter finds only them.

    Returns the children. The label is the scope: a node carrying it is the
    harness's to claim, and a node that loses it is not.
    """
    children = dg.list(parent=epic_ref)
    for node in children:
        labels = set(node.get("labels", []))
        if not is_(node["status"], "STATUS_", "open") or label in labels or BLOCKED_LABEL in labels:
            continue
        dg.set_labels(node, labels | {label})
        say(f"  .. labelled {dg.ref(node)} {label}")
    return children


def predecessors(dg, node, by_id):
    """Results of the done nodes that block this one, oldest first, from dagger's
    own `inputs` — which crosses projects. A lookup over this project's node list
    misses a blocker filed in another project and drops its result silently."""
    out = []
    for item in dg.run("inputs", dg.ref(node)).get("inputs", []):
        if item.get("result"):
            out.append({"node": item["node"], "result": item["result"]})
    return sorted(out, key=lambda s: int(s["node"]["number"]))


def short(value, prefix):
    """PRIORITY_P2 -> p2; p2 -> p2. Accepts both enum wire forms."""
    return value[len(prefix):].lower() if value and value.startswith(prefix) else value


def is_(value, prefix, want):
    """True when an enum field is `want` in either wire form."""
    return short(value, prefix) == want


def work_order_text(dg, node, by_id):
    buf = io.StringIO()
    w = buf.write
    w(f"# {dg.ref(node)}: {node['title']}\n\n")
    w(f"kind: {node.get('kind', 'task')}  priority: {short(node.get('priority'), 'PRIORITY_')}\n\n")
    w(node.get("body", "").rstrip() + "\n")
    preds = predecessors(dg, node, by_id)
    if preds:
        w("\n# Inputs from predecessors\n\n")
        w("The results of the nodes this one waited on. They say what was built and where.\n")
        for shown in preds:
            p = shown["node"]
            result = (shown.get("result") or {}).get("body")
            w(f"\n## {p.get('reference') or dg.ref(p)}: {p['title']}\n\n")  # its own project, not ours
            w((result or "(completed with no result body)").rstrip() + "\n")
    return buf.getvalue()


def is_grouping(node, children):
    """A node that groups work rather than being work.

    It has children and states no acceptance criteria, so there is nothing for
    an agent to satisfy and nothing for a reviewer to judge. Dagger makes it
    ready once its children close; the honest completion is bookkeeping.
    """
    return bool(children) and "## Acceptance criteria" not in node.get("body", "")


# --- local state: attempts and usage, until dagger takes usage reports ---------


def load_state():
    if STATE.exists():
        return json.loads(STATE.read_text())
    return {"nodes": {}, "history": []}


def save_state(state):
    STATE.write_text(json.dumps(state, indent=2) + "\n")


def record(state, ref, attempt, role, usage, turns, verdict):
    """One usage record per node, attempt and role. Same shape dagger stores."""
    state["history"].append({
        "at": now(), "node": ref, "attempt": attempt, "role": role,
        "verdict": verdict, "turns": turns, "usage": usage,
    })


def entry_cost(entry):
    return sum(u["cost_usd"] for u in entry.get("usage", [])) or entry.get("cost_usd", 0.0)


def total_cost(state):
    return sum(entry_cost(e) for e in state["history"])


def node_state(state, ref):
    return state["nodes"].setdefault(ref, {"status": "open", "attempts": 0})


# --- git ---------------------------------------------------------------------


def git(*args, check=True):
    return subprocess.run(
        ["git", *args], cwd=ROOT, check=check, capture_output=True, text=True
    ).stdout.strip()


def changed_paths():
    return [line[3:].strip().strip('"') for line in git("status", "--porcelain").splitlines()]


def working_tree_dirty():
    """True when the implementer changed something, ignoring harness bookkeeping."""
    return any(not p.startswith(HARNESS_RUNTIME) for p in changed_paths())


def tree_snapshot():
    """Paths already modified before an agent runs. Not the agent's doing."""
    return set(changed_paths())


def touched_protected(baseline=frozenset()):
    return [p for p in changed_paths()
            if p not in baseline and not p.startswith(HARNESS_RUNTIME) and p.startswith(PROTECTED)]


def head():
    return git("rev-parse", "HEAD")


def undo_agent_commits(before):
    """Keep the work, drop the commit. The harness owns the history.

    Only commits that are not on the remote are the agent's to undo. A commit
    that reached origin while the agent ran was made by someone else, and
    resetting it once diverged local history and broke every later push.
    """
    if head() == before:
        return
    on_origin = subprocess.run(
        ["git", "merge-base", "--is-ancestor", "HEAD", "origin/main"], cwd=ROOT
    ).returncode == 0
    if on_origin:
        say("  .. HEAD moved to a commit already on origin; leaving it")
        return
    git("reset", "--soft", before)
    say("  .. agent committed; un-committed, harness owns history")


def commit_and_push(ref, node, summary, args):
    git("add", "-A")
    subject = f"{ref}: {node['title']}"
    body = (
        f"Implements {ref}.\n\n"
        "Reviewed against the node's acceptance criteria and scenarios.\n\n"
        f"Claude-Session: {SESSION_URL}"
    )
    # The pre-commit hooks have failed once on a transient, so one retry, and
    # the hook's own words on the second failure rather than a bare exit code.
    for attempt in (1, 2):
        r = subprocess.run(
            ["git", "commit", "-m", subject, "-m", body], cwd=ROOT,
            capture_output=True, text=True,
        )
        if r.returncode == 0:
            break
        say(f"  !! git commit failed (attempt {attempt}):\n{r.stderr.strip()}\n{r.stdout.strip()}")
        if attempt == 2:
            raise RuntimeError(f"git commit failed for {ref}; the work is staged in the tree")
        time.sleep(5)
    sha = head()
    say(f"  ++ committed: {sha[:10]} {subject}")
    if not args.no_push:
        r = subprocess.run(["git", "push"], cwd=ROOT, capture_output=True, text=True)
        say("  ++ pushed" if r.returncode == 0 else f"  !! push failed: {r.stderr.strip()}")
    return sha


# --- agents ------------------------------------------------------------------


def task_output_path(session_id, task_id):
    """Where the CLI writes a background task's output for this checkout."""
    slug = str(ROOT).replace("/", "-").replace(".", "-")
    return Path(f"/tmp/claude-{os.getuid()}") / slug / session_id / "tasks" / f"{task_id}.output"


def output_size(path):
    try:
        return path.stat().st_size
    except OSError:
        return -1


def descendants(pid):
    """Every process under pid, deepest first, from /proc."""
    out = []
    try:
        children = Path(f"/proc/{pid}/task/{pid}/children").read_text().split()
    except OSError:
        return out
    for child in children:
        out.extend(descendants(int(child)))
        out.append(int(child))
    return out


def cli_pid(client):
    """The CLI subprocess behind a client, when the transport exposes it."""
    transport = getattr(client, "_transport", None)
    process = getattr(transport, "_process", None)
    return getattr(process, "pid", None)


def kill_tasks(client, log):
    pid = cli_pid(client)
    if pid is None:
        log.line("  !! breaker: the CLI's pid is not exposed; nothing killed")
        return
    for child in descendants(pid):
        try:
            os.kill(child, signal.SIGTERM)
        except OSError:
            continue


POLL_LOOP = re.compile(r"\b(until|while)\b[^\n]*\bpgrep\s+-f|\bpgrep\s+-f[^\n]*;\s*do\b")


async def deny_poll_loops(hook_input, tool_use_id, context):
    """PreToolUse on Bash: a loop that polls with `pgrep -f` matches the shell
    running the loop, so it never ends. Backgrounding stays allowed; the wait
    has a tool."""
    command = (hook_input.get("tool_input") or {}).get("command", "")
    if not POLL_LOOP.search(command):
        return {}
    return {"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "deny",
        "permissionDecisionReason": (
            "A loop that polls with `pgrep -f` matches the shell running the loop and "
            "never ends. Wait on a background task with TaskOutput, or end your turn: "
            "the task's completion wakes you."),
    }}


async def run_agent(kind, system_prompt, prompt, log_path, args):
    """Run one agent to completion. Returns (final_text, cost, turns, usage).

    Completion is not the first result message. An agent may background a
    command, spawn a subagent, or set a monitor and end its turn to wait; the
    CLI wakes it when the task finishes and it takes another turn. So the
    session stays open until a turn ends with no task the CLI reported as
    started still running. A single-shot query would return at the first turn
    and lose everything after it, which is how the first live run lost an
    implementer mid-`make test`.
    """
    options = ClaudeAgentOptions(
        system_prompt=system_prompt,
        cwd=str(ROOT),
        permission_mode="auto",
        setting_sources=["project"],
        model=args.model,
        effort=args.effort,
        max_turns=200,
        env={"BASH_DEFAULT_TIMEOUT_MS": BASH_TIMEOUT_MS},
        hooks={"PreToolUse": [HookMatcher(matcher="Bash", hooks=[deny_poll_loops])]},
    )
    if kind == "reviewer":
        # A reviewer that can edit is a reviewer that fixes what it should fail.
        options.disallowed_tools = ["Write", "Edit", "NotebookEdit"]

    texts, cost, turns, usage = [], 0.0, 0, []
    active = {}  # task id -> description, for tasks the CLI will wake the agent for
    with Tail(log_path, kind) as log:
        log.section(f"{kind} @ {now()}")
        async with ClaudeSDKClient(options=options) as client:
            await client.query(prompt)
            stream = client.receive_messages()
            session_id = None
            waiting_since = None   # when a turn ended with tasks still active
            heartbeat_turn = False # the current turn answers a heartbeat, not the work
            strikes = 0
            seen = {}              # task id -> output size at the last look
            final = ""             # the last text of the last working turn
            while True:
                try:
                    timeout = HEARTBEAT if waiting_since else None
                    message = await asyncio.wait_for(stream.__anext__(), timeout=timeout)
                except StopAsyncIteration:
                    break
                except asyncio.TimeoutError:
                    if not active:
                        break
                    busy = []
                    for task_id, description in active.items():
                        size = output_size(task_output_path(session_id or "", task_id))
                        if size > seen.get(task_id, -1):
                            busy.append(description)
                        seen[task_id] = size
                    if busy:
                        log.line(f"  .. heartbeat: still producing output: {'; '.join(busy)}")
                        continue
                    strikes += 1
                    minutes = int((time.time() - waiting_since) // 60)
                    if strikes < HEARTBEAT_STRIKES:
                        log.line(f"  .. heartbeat {strikes}/{HEARTBEAT_STRIKES}: no output from "
                                 + "; ".join(active.values()) + "; asking")
                        heartbeat_turn = True
                        await client.query(HEARTBEAT_PROMPT.format(
                            minutes=minutes, tasks="; ".join(active.values()),
                            strike=strikes, strikes=HEARTBEAT_STRIKES))
                        continue
                    log.line(f"  !! breaker: silent through {HEARTBEAT_STRIKES} heartbeats: "
                             + "; ".join(active.values()) + "; killing the task's processes")
                    kill_tasks(client, log)
                    active.clear()
                    break
                if isinstance(message, AssistantMessage):
                    for block in message.content:
                        if isinstance(block, ToolUseBlock):
                            for line in describe_tool(block):
                                log.line(line)
                        elif isinstance(block, TextBlock):
                            texts.append(block.text)
                            log.text(block.text)
                elif isinstance(message, UserMessage) and not isinstance(message.content, str):
                    for block in message.content:
                        if isinstance(block, ToolResultBlock):
                            log.line(describe_result(block))
                elif isinstance(message, TaskStartedMessage):
                    session_id = session_id or message.session_id
                    active[message.task_id] = message.description
                    seen[message.task_id] = output_size(task_output_path(session_id, message.task_id))
                    log.line(f"task started: {message.description}")
                elif isinstance(message, TaskNotificationMessage):
                    active.pop(message.task_id, None)
                    log.line(f"task {message.status}: {message.summary}")
                elif isinstance(message, TaskUpdatedMessage):
                    if (message.patch or {}).get("status") in TERMINAL_TASK_STATUSES:
                        log.line(f"task ended: {active.pop(message.task_id, message.task_id)}")
                elif isinstance(message, ResultMessage):
                    session_id = session_id or message.session_id
                    # Totals are cumulative across the session's turns, so the
                    # latest result message carries the whole session.
                    cost = message.total_cost_usd or 0.0
                    turns += message.num_turns or 0
                    usage = []
                    # Recorded in the shape of dagger's own Usage entity: counts per
                    # model, never currency. Cost is derived from a rate table.
                    for model_name, mu in (message.model_usage or {}).items():
                        usage.append({
                            "model": model_name,
                            "input_tokens": mu.get("inputTokens", 0),
                            "output_tokens": mu.get("outputTokens", 0),
                            "cache_read_tokens": mu.get("cacheReadInputTokens", 0),
                            "cache_write_tokens": mu.get("cacheCreationInputTokens", 0),
                            "cost_usd": round(mu.get("costUSD", 0.0), 4),
                        })
                    if message.is_error:
                        log.line(f"ERROR: {message.result}")
                    # A heartbeat's answer is not the work's summary.
                    if not heartbeat_turn and texts:
                        final = texts[-1].strip()
                    heartbeat_turn = False
                    if active:
                        waiting_since = waiting_since or time.time()
                        log.line(f"turn ended; waiting on {len(active)} task(s): "
                                 + "; ".join(active.values()))
                        continue
                    log.line(f"done: {turns} turns, ${cost:.2f}")
                    break
        if not final and texts:
            final = texts[-1].strip()
        log.section("final")
        log.text(final)
    return final, cost, turns, usage


class Tail:
    """What an agent is doing, as it does it, for `tail -f harness/logs/loop.log`.

    Every line goes to the per-node log, the run-wide log, and stdout, and is
    flushed at once. Text is the agent's own words; tool lines carry the
    agent's stated intent and the command, never the full output.
    """

    def __init__(self, node_log, kind):
        self.paths = (node_log, LOGS / "loop.log")
        self.tag = {"implementer": "impl", "reviewer": "review"}.get(kind, kind)

    def __enter__(self):
        self.files = [open(p, "a") for p in self.paths]
        return self

    def __exit__(self, *exc):
        for f in self.files:
            f.close()

    def _emit(self, s):
        for f in self.files:
            f.write(s)
            f.flush()
        print(s, end="", flush=True)

    def section(self, title):
        self._emit(f"\n===== {title} =====\n")

    def line(self, s):
        self._emit(f"  [{self.tag}] {s}\n")

    def text(self, s):
        s = s.strip()
        if s:
            self._emit("\n" + "\n".join(f"  {self.tag}> {ln}" for ln in s.splitlines()) + "\n")


def describe_tool(block):
    """The agent's intent, then what it ran. Bash carries its own description."""
    name, inp = block.name, block.input or {}
    if name == "Bash":
        cmd = " ".join(str(inp.get("command", "")).split())
        lines = []
        if inp.get("description"):
            lines.append(f"-- {inp['description']}")
        lines.append(f"$ {cmd[:200]}")
        return lines
    for key in ("file_path", "pattern", "path", "query"):
        if key in inp:
            return [f"{name}: {inp[key]}"]
    return [name]


def describe_result(block):
    """One line per tool result: outcome and size, not content."""
    content = block.content
    if isinstance(content, list):
        content = " ".join(part.get("text", "") for part in content if isinstance(part, dict))
    content = (content or "").strip()
    first = content.splitlines()[0][:120] if content else ""
    status = "error" if block.is_error else "ok"
    return f"   = {status}, {len(content):,} chars" + (f": {first}" if first else "")


def parse_verdict(text):
    """Return (passed, feedback). Absent or malformed verdict is a fail."""
    lines = [ln.rstrip() for ln in text.strip().splitlines()]
    for line in reversed(lines):
        if line.strip().upper().startswith("VERDICT:"):
            passed = "PASS" in line.upper()
            feedback = "\n".join(ln for ln in lines if ln.strip().startswith("- "))
            return passed, feedback or text.strip()
    return False, "The reviewer returned no VERDICT line.\n\n" + text.strip()


async def keep_alive(dg, lease_id):
    """Renew the lease while an agent runs. Cancelled when the node is finished."""
    while True:
        await asyncio.sleep(HEARTBEAT_EVERY)
        try:
            dg.heartbeat(lease_id)
        except RuntimeError as e:
            say(f"  !! heartbeat failed: {e}")


async def with_lease(dg, lease_id, coro, timeout):
    task = asyncio.create_task(keep_alive(dg, lease_id))
    try:
        return await asyncio.wait_for(coro, timeout=timeout)
    finally:
        task.cancel()


# --- one node ----------------------------------------------------------------


async def do_node(dg, node, lease, label, state, args):
    ref = dg.ref(node)
    lease_id = lease["id"]
    ns = node_state(state, ref)
    by_id = {n["id"]: n for n in dg.list()}
    children = dg.list(parent=ref)

    if is_grouping(node, children):
        say(f"\n  {ref}  {node['title']}  (grouping; children complete)")
        dg.complete(ref, lease_id, "Grouping node: every child completed. See their results.")
        ns["status"] = "done"
        record(state, ref, 0, "harness", [], 0, "grouping")
        save_state(state)
        return True

    order = work_order_text(dg, node, by_id)
    LOGS.mkdir(parents=True, exist_ok=True)
    log_path = LOGS / f"{int(node['number']):03d}-{node['title'][:40].replace('/', '-')}.log"

    say(f"\n{'=' * 70}\n  {ref}  {node['title']}\n{'=' * 70}")

    feedback = ns.get("feedback")
    summary = ""
    for attempt in range(ns["attempts"] + 1, args.attempts + 1):
        ns["attempts"] = attempt
        ns["status"] = "in_progress"
        save_state(state)
        say(f"\n  -- implement (attempt {attempt}/{args.attempts})")

        prompt = f"# Work order\n\n{order}\n"
        if feedback:
            prompt += (
                "\n# A previous attempt was rejected\n\n"
                "The reviewer found these parts of the contract unsatisfied. "
                "Fix them. Do not start over; build on what is already there.\n\n"
                f"{feedback}\n"
            )
        before, baseline = head(), tree_snapshot()
        try:
            summary, cost_i, turns_i, usage_i = await with_lease(
                dg, lease_id,
                run_agent("implementer", IMPLEMENTER_PROMPT, prompt, log_path, args),
                args.timeout,
            )
        except asyncio.TimeoutError:
            say(f"  !! implementer exceeded {args.timeout}s")
            undo_agent_commits(before)
            record(state, ref, attempt, "implementer", [], 0, "timeout")
            feedback = f"- The previous attempt ran past {args.timeout}s and was stopped. Work in smaller steps."
            continue
        undo_agent_commits(before)

        protected = touched_protected(baseline)
        if protected:
            feedback = "- The implementation modified protected paths: " + ", ".join(
                protected
            ) + ". Revert them and satisfy the contract in application code only."
            say(f"  !! touched protected paths: {protected}")
            for path in protected:
                git("checkout", "--", path, check=False)
            continue

        if not working_tree_dirty():
            feedback = "- The attempt produced no changes at all."
            say("  !! no changes produced")
            continue

        say(f"\n  -- review (attempt {attempt}/{args.attempts})")
        diff = git("diff", "--stat", "HEAD")
        review_prompt = (
            f"# Node under review\n\n{order}\n\n"
            f"# Changes in the working tree\n\n```\n{diff}\n```\n\n"
            "Inspect the working tree, run the suite, and judge the contract."
        )
        try:
            verdict_text, cost_r, turns_r, usage_r = await with_lease(
                dg, lease_id,
                run_agent("reviewer", REVIEWER_PROMPT, review_prompt, log_path, args),
                args.timeout,
            )
        except asyncio.TimeoutError:
            say(f"  !! reviewer exceeded {args.timeout}s; treating as fail")
            verdict_text, cost_r, turns_r, usage_r = "VERDICT: FAIL", 0.0, 0, []
        passed, feedback = parse_verdict(verdict_text)

        record(state, ref, attempt, "implementer", usage_i, turns_i, "n/a")
        record(state, ref, attempt, "reviewer", usage_r, turns_r,
               "pass" if passed else "fail")

        if passed:
            say(f"  == PASS  (${cost_i + cost_r:.2f}, {turns_i + turns_r} turns)")
            sha = commit_and_push(ref, node, summary, args)
            result = f"{summary.rstrip()}\n\nCommit: {sha}\nFiles:\n{diff}\n"
            dg.complete(ref, lease_id, result)
            ns["status"] = "done"
            ns.pop("feedback", None)
            save_state(state)
            say(f"  ++ completed {ref}")
            return True

        say(f"  == FAIL\n{feedback}")
        ns["feedback"] = feedback
        save_state(state)

    # Out of attempts. Leave the findings on the node, take it out of scope,
    # and give the lease back so the graph is honest about who holds what.
    ns["status"] = "blocked"
    ns["feedback"] = feedback
    save_state(state)
    dg.comment(ref, lease_id,
               f"harness: blocked after {args.attempts} attempts. Last review:\n\n{feedback}\n\n"
               f"Relabel `{label}` to retry.")
    labels = (set(node.get("labels", [])) - {label}) | {BLOCKED_LABEL}
    dg.set_labels(node, labels, lease_id)
    dg.release(lease_id, "budget", f"{args.attempts} attempts exhausted; see comment")
    return False


# --- report ------------------------------------------------------------------


def report(state, titles):
    """Per-node token and cost tally. Calibration data for future estimates."""
    per = {}
    for e in state["history"]:
        a = per.setdefault(e["node"], {"in": 0, "out": 0, "cr": 0, "cw": 0,
                                       "cost": 0.0, "turns": 0, "attempts": 0})
        if not e.get("usage"):
            a["cost"] += e.get("cost_usd", 0.0)
        for u in e.get("usage", []):
            a["in"] += u["input_tokens"]
            a["out"] += u["output_tokens"]
            a["cr"] += u["cache_read_tokens"]
            a["cw"] += u["cache_write_tokens"]
            a["cost"] += u["cost_usd"]
        a["turns"] += e.get("turns", 0)
        a["attempts"] = max(a["attempts"], e.get("attempt", 0))

    print(f"\n{'=' * 96}\n  per-node usage\n{'=' * 96}")
    print(f"{'node':<12} {'status':<8} {'try':>3} {'turns':>5} {'input':>9} {'output':>8} "
          f"{'cache rd':>10} {'cache wr':>9} {'cost':>8}  title")
    print("-" * 96)
    tot = {"in": 0, "out": 0, "cr": 0, "cw": 0, "cost": 0.0, "turns": 0}
    for ref in sorted(per, key=lambda r: (r.rsplit("#", 1)[0], int(r.rsplit("#", 1)[1]) if "#" in r else 0)):
        a = per[ref]
        for k in tot:
            tot[k] += a[k]
        st = state["nodes"].get(ref, {}).get("status", "?")
        print(f"{str(ref):<12} {st:<8} {a['attempts']:>3} {a['turns']:>5} "
              f"{a['in']:>9,} {a['out']:>8,} {a['cr']:>10,} {a['cw']:>9,} "
              f"${a['cost']:>7.2f}  {titles.get(str(ref), '')[:30]}")
    print("-" * 96)
    print(f"{'TOTAL':<12} {'':<8} {'':>3} {tot['turns']:>5} {tot['in']:>9,} {tot['out']:>8,} "
          f"{tot['cr']:>10,} {tot['cw']:>9,} ${tot['cost']:>7.2f}")

    billable = tot["in"] + tot["out"] + tot["cr"] + tot["cw"]
    done = sum(1 for v in state["nodes"].values() if v["status"] == "done")
    blocked = sorted(n for n, v in state["nodes"].items() if v["status"] == "blocked")
    print(f"\n  {done} nodes done" + (f", blocked: {', '.join(blocked)}" if blocked else ""))
    print(f"  {billable:,} tokens across every agent and role")
    print(f"  ${tot['cost']:.2f} at list prices; included in a subscription")
    if done:
        print(f"  ${tot['cost'] / done:.2f} and {billable // done:,} tokens per completed node")


# --- main --------------------------------------------------------------------


async def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--project", default=os.environ.get("DG_PROJECT", "dagger"),
                   help="project slug (default: $DG_PROJECT, then dagger)")
    p.add_argument("--epic", default=None,
                   help="the grouping node whose open children are the scope, as #N")
    p.add_argument("--max", type=int, default=100, help="max nodes this run")
    p.add_argument("--attempts", type=int, default=3, help="review attempts per node")
    p.add_argument("--model", default="claude-opus-5", help="model (default: Opus 5)")
    p.add_argument("--effort", default="medium",
                   choices=["low", "medium", "high", "xhigh", "max"],
                   help="reasoning effort (default: medium)")
    p.add_argument("--budget", type=float, default=None,
                   help="stop once total spend passes this many USD")
    p.add_argument("--timeout", type=int, default=3600,
                   help="seconds before one agent is abandoned (default: 3600)")
    p.add_argument("--dry-run", action="store_true", help="show the ready set and exit")
    p.add_argument("--report", action="store_true", help="print per-node usage and exit")
    p.add_argument("--no-push", action="store_true", help="commit but do not push")
    args = p.parse_args()

    if STOP.exists():
        STOP.unlink()
    state = load_state()
    dg = Dagger(args.project)

    if args.report:
        titles = {dg.ref(n): n["title"] for n in dg.list()}
        report(state, titles)
        return 0

    if not args.epic:
        p.error("--epic is required: the node whose children are the scope")
    epic_ref = args.epic if "#" in args.epic and not args.epic.startswith("#") else f"{args.project}{args.epic}"
    label = f"epic-{epic_ref.rsplit('#', 1)[1]}"
    session = f"loop-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}"

    say(f"{epic_ref}: scoping by label {label}")
    children = scope_epic(dg, epic_ref, label)
    open_children = [c for c in children if is_(c["status"], "STATUS_", "open")]

    if args.dry_run:
        ready = dg.ready(label)
        done = sum(1 for c in children if is_(c["status"], "STATUS_", "done"))
        print(f"\n{epic_ref}: {done} done, {len(ready)} ready of {len(children)} children\n")
        for n in ready:
            print(f"  {short(n.get('priority'), 'PRIORITY_')}  {dg.ref(n)}  {n['title']}")
        waiting = [c for c in open_children if is_(c.get("assignee_kind"), "ASSIGNEE_KIND_", "user")]
        for n in waiting:
            print(f"  gate  {dg.ref(n)}  {n['title']}  (waiting on a person)")
        return 0

    total = 0.0
    for i in range(1, args.max + 1):
        node, lease = dg.claim(label, session)
        if node is None:
            children = dg.list(parent=epic_ref)
            still_open = [dg.ref(c) for c in children if is_(c["status"], "STATUS_", "open")]
            blocked = [dg.ref(c) for c in children if BLOCKED_LABEL in c.get("labels", [])]
            titles = {dg.ref(n): n["title"] for n in children}
            if blocked:
                STOP.write_text(f"BLOCKED: {blocked} exhausted their attempts\n")
                print(f"\nBLOCKED: {blocked}")
                report(state, titles)
                return 2
            if still_open:
                STOP.write_text(f"WAITING: {still_open} open but not claimable\n")
                print(f"\nWAITING: nothing claimable; still open: {still_open}")
                report(state, titles)
                return 0
            STOP.write_text("DONE: the epic's children are all closed\n")
            print("\nDONE: epic drained")
            report(state, titles)
            return 0

        ref = dg.ref(node)
        try:
            ok = await do_node(dg, node, lease, label, state, args)
        except BaseException as e:
            dg.release(lease["id"], "crash", f"harness: {type(e).__name__}: {e}"[:500])
            raise
        total = total_cost(state)
        say(f"\n  ${total:.2f} spent this state file")
        if args.budget and total >= args.budget:
            STOP.write_text(f"BLOCKED: budget ${args.budget} reached at ${total:.2f}\n")
            print(f"\nStopping: budget ${args.budget:.2f} reached")
            return 2
        if not ok:
            say(f"  {ref} blocked after {args.attempts} attempts; moving on")
        time.sleep(2)

    print(f"\nReached --max ({args.max}). Total: ${total:.2f}")
    return 1


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except KeyboardInterrupt:
        print("\ninterrupted")
        sys.exit(1)
