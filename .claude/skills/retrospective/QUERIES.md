# Retrospective: data recipes

`S=../cli/stead`; `M='--socket /run/mill/mill.sock'`; `SINCE` is the window start in UTC, e.g. `2026-09-20T11:00:00Z`. Write intermediate files to the session scratchpad, not the repository.

## Merged pull requests and lines

```bash
for r in mill dagger cli steading; do
  gh pr list -R steading-ai/$r --state merged --limit 100 --json number,additions,deletions,changedFiles,mergedAt,headRefName,title \
    --jq ".[] | select(.mergedAt >= \"$SINCE\") | \"$r\t#\(.number)\t\(.headRefName)\t\(.mergedAt)\t\(.additions)\t\(.deletions)\t\(.changedFiles)\t\(.title)\""
done | sort -t$'\t' -k4 > prs.tsv
awk -F'\t' '{a[$1]+=$5; d[$1]+=$6; n[$1]++} END {for (r in a) printf "%s\tprs=%d\t+%d\t-%d\n", r, n[r], a[r], d[r]}' prs.tsv
```

Lines by class, from each PR's file list:

```bash
while IFS=$'\t' read -r repo num rest; do n=${num#\#}
  gh pr view $n -R steading-ai/$repo --json files --jq ".files[] | \"$repo\t$n\t\(.path)\t\(.additions)\t\(.deletions)\""
done < prs.tsv > files.tsv
awk -F'\t' '{p=$3; c="source"; if (p ~ /(^|\/)gen\//) c="generated"; else if (p ~ /pnpm-lock|go\.sum|package-lock/) c="lockfile"; else if (p ~ /^docs\/plans\//) c="plan docs"; else if (p ~ /_test\.go$|\.test\.ts$|\.spec\.ts$|testdata\//) c="tests"; else if (p ~ /^docs\/|\.md$/) c="docs"; a[c]+=$4; d[c]+=$5} END {for (c in a) printf "%s\t+%d\t-%d\n", c, a[c], d[c]}' files.tsv | sort
```

## Nodes

```bash
for p in mill dagger stead steading; do
  echo "$p done=$(dg node list -p $p --status done --view summary | jq '[.nodes[] | select(.title|test("stopped after")|not)] | length') gates=$(dg node list -p $p --status done --view summary | jq '[.nodes[] | select(.title|test("stopped after"))] | length') open=$(dg node list -p $p --status open --view summary | jq '.nodes|length')"
done
```

`dg node list` carries no completion time; the done count is since the project's creation. Say so if the window is shorter.

## Tokens

```bash
$S shell -- mill cost --since 48h --view full --bucket day $M > cost.txt
grep -n '^[A-Z][A-Z ]*  ' cost.txt                    # RUN, WORKFLOW, STEP, MODEL, DAY sections
awk '/^RUN /{s=1;next} /^$/{s=0} s && $2=="gateway"{i+=$3;o+=$4;cr+=$5;cw+=$6;n++} END{printf "runs=%d input=%d output=%d cache_read=%d cache_write=%d\n",n,i,o,cr,cw}' cost.txt
awk '/^MODEL/{s=1} /^DAY/{s=0} s' cost.txt
awk '/^DAY/{s=1} s' cost.txt
awk '/^STEP/{s=1} /^MODEL/{s=0} s && /<workflow-hash>/ && $2=="gateway"' cost.txt | awk '{printf "%-38s reached=%s passed=%s died=%s retried=%s revises=%s attempts=%s out=%s\n",$1,$8,$9,$10,$11,$12,$13,$4}'
```

`gateway` rows are authoritative; `sdk` rows overlap them and are not added. The USD column is a dash for models the rate table lacks; the API-rate equivalent in the document is `input×in + output×out + cache_read×cr + cache_write×cw` per model at published rates, labelled as arithmetic. `--since` accepts hours (`48h`, `212h`); `--until` and `--bucket` exist from mill dev.150 on.

## Waste

On the guest, the ledger:

```bash
$S shell -- bash -c 'q() { sudo -u postgres psql -d mill -Atc "$1"; }
q "select status, count(*) from step_runs group by status order by 2 desc"
q "select after->>'"'"'reason'"'"', count(*) from events where op='"'"'abort_step_run'"'"' group by 1 order by 2 desc"
q "select count(*) from events where op='"'"'fail_step_run'"'"'"'
```

Redone work is `superseded + aborted + failed` over all step runs.

## The week before

The server keeps last week's ledger. Its mill may be older: no `--until`, no `--bucket`.

```bash
ssh core 'mill cost --since 212h --view full --socket /run/user/1000/mill/mill.sock' > server.txt   # 212h ≈ the previous reset, when nothing has run there since
```

Same awk as above for runs, gateway totals and MODEL. Per-run figures (output, cache read, cost) are the comparison that survives a change of model or host.

## The supervisor's own bill

```bash
ccstat ~/.claude/projects/<project-dir>/<session-id>.jsonl
```

Cache reads to output tokens is the ratio to report; the subagent block reports fork transcripts it cannot find as failures.
