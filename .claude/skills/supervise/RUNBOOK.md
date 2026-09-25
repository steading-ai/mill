# Supervise: runbook

Recipes the skill body refers to. Read the section you need. `S=../cli/stead` throughout; `M='--socket /run/mill/mill.sock'`.

## Orient

```bash
$S shell -- mill status --view full $M                      # live runs; LEASES/ROUNDS columns
$S shell -- mill explain '<ref>' $M                          # what a run waits on, gate id when parked
$S shell -- mill inspect '<ref>' $M | awk '/^(implement|review|merge) /'   # attempts per step
$S shell -- mill history '<ref>' $M | tail -12               # ledger ops with timestamps
$S shell -- mill doctor $M                                   # version, migrations, workflow store vs disk
skift ready -p <project> --view refs                            # per project: mill skift stead steading
$S shell -- sudo journalctl -u mill.service --utc --since "2 hours ago" --no-pager -o cat | grep -v 'gateway:\|recording activity'
```

Journal `--since` takes `"YYYY-MM-DD HH:MM:SS UTC"` with `--utc`; a `Z` suffix returns nothing. Guest times print in PDT unless `--utc`.

## Monitor

Diff of `mill status` once a minute; prints picked up / changed / gone. A selecting run has no step column, so its clock is masked.

```bash
cd ../cli
prev=""
while true; do
  cur=$(./stead shell -- mill status --socket /run/mill/mill.sock --view full 2>/dev/null | tail -n +3 | awk '{ if ($3=="selecting") print $1, $3, "-", "-", "-"; else print $1, $3, $4, $5, $7 }' | sort)
  if [ -n "$prev" ] && [ "$cur" != "$prev" ]; then
    comm -13 <(echo "$prev") <(echo "$cur") | sed 's/^/changed\/picked up: /'
    comm -23 <(echo "$prev") <(echo "$cur") | awk '{print $1}' | sort -u > /tmp/.gone.$$
    comm -13 <(echo "$cur" | awk '{print $1}' | sort -u) /tmp/.gone.$$ | sed 's/^/gone: /'
    rm -f /tmp/.gone.$$
  fi
  prev=$cur; sleep 60
done
```

Arm with `timeout_ms: 1800000`; re-arm on expiry while anything is live. A single last run is cheaper to wait for with a background `until` loop than a monitor.

## Logs

```bash
$S shell -- mill logs '<ref>' --step review --attempt N $M --no-pager | sed -n '/^-- review attempt N ended/,$p' | grep -E '^\s*[-*]|^-- ' | head -6
$S shell -- mill logs '<ref>' --step implement --attempt N $M --no-pager | grep -E '^[0-9T:Z-]+ text' | tail -1   # last words, truncated
```

Log text lines are truncated; the full final message is in `/var/lib/mill/logs/<run>/<step>.<attempt>.activity[.zst]`, binary framed: `zstdcat … | LC_ALL=C tr -c '[:print:]\n' '\n' | awk 'length>20'`. No `strings` in the guest.

Empty-session signature (mill#34): prompt begin/end only, zero `usage|tool|result` lines, exit 0. Outage signature: `-- <step> attempt N ended: API Error: Repeated 529 …` or `500 Internal server error`; the gateway logs only `no usage on the response`.

## Gates

`mill explain` names the gate id; the gate node is `<project>#N` titled `<ref>: <step> stopped after N attempts`. Answer with `skift complete '<gate>' --result "…"`. Drop a stale one with `skift drop '<gate>' --reason no_longer_relevant`.

| Cause, from the gate body and the step log | Whose | Action |
|---|---|---|
| Merge conflict, or catch-up overlap the session could not resolve | supervisor's | Merge main into the branch (a subagent, or a scratch worktree), push, reset the worker checkout (§Checkout), complete the gate |
| Every round is the same upstream error (529, 500, unreachable) | supervisor's, once the upstream answers | Wait for a real request to succeed in the journal (`gateway: usage … model=claude-opus`), then complete. Completing into a live outage burns the next three attempts in a minute |
| Retries spent on real findings the implementer did not address | person's | Summarise the rounds, say what the implementer said it could not do, recommend |
| A decision the session declined to make (credentials, supply chain, scope) | person's | Lay out the requirement in its own words, then the options with costs, then a recommendation |
| The node is already merged (the run outlived its PR) | supervisor's | Cancel the run, complete the node by hand with the merge commit, drop the gate |

After a resume the step gets fresh attempts, and an outage that ate the retries has also eaten the revise rounds behind them; expect one more round than usual.

## Checkout

A worker checkout the branch has moved past:

```bash
git bundle create /tmp/x.bundle origin/<branch>            # a bare sha refuses: use the ref
$S shell -- bash -c 'cat > /tmp/x.bundle' < /tmp/x.bundle
$S shell -- sudo -u ivy bash -c "cd /var/lib/mill/workspaces/<runid>/checkout && git fetch -q /tmp/x.bundle refs/remotes/origin/<branch> && git reset -q --hard FETCH_HEAD"
```

The checkout is owned by the guest user, not `mill`. Commits to mill on the Mac need `HK_SKIP_STEPS=test-unit`. The classifier blocks `gh pr merge` and force-pushes; do not retry, report.

## Filing

`skift create -p <project> --kind bug --priority pN --body "$(cat <<'EOF' … EOF)" "<title>" --view refs`. Body: **What happened** with UTC timestamps, run and step-run ids, the journal lines verbatim, and what the other slot was doing at the time; **Why**, citing `file:line` in the deployed version (`mill doctor` prints it); **Acceptance criteria**; **Scenario** in Gherkin. No hostnames. p0 when main can be harmed, p1 when a run is lost or a cap can cascade, p2 otherwise. Comment occurrences on an existing node: `skift comment '<ref>' "<body>"` (body is positional).

Ledger queries when the journal is not enough, on the guest: `sudo -u postgres psql -d mill -Atc "…"` (`step_runs`, `events`, `usage`); skift's is `-d skift` (`leases`, `events` with `op`, `reason`, `request_id`).

## Cap

Hold every ready node so nothing new claims: `for n in $(skift ready -p <p> --view refs | jq -r '.nodes[].reference'); do skift hold "$n" --reason "…"; done` across projects. A 429 from the cap is a 4xx to mill and walks the ready set into park gates (mill#63); holding is the stopgap. Stopping the worker is `sudo systemctl stop mill.service` and is the person's call; a stop aborts attempts as `shutdown`, a free retry.

## Restarts

Every merge to mill main redeploys the worker through the follower and aborts in-flight attempts (`abort_step_run reason=shutdown`); the runs resume at the aborted step with a free retry. A run cut between claim and selection sits in `selecting` until a slot frees. Nodes filed for it: mill#44 (done), drain-before-deploy (open).
