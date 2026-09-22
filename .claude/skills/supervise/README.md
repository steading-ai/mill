# `/supervise` — Watch the Factory, Answer What Is Yours to Answer

Keeps an eye on the mill worker in the stead guest for hours at a stretch: reports each run by node title as it moves, answers the park gates a supervisor may answer, files bugs from the record, and hands the person only what needs a person.

```
/supervise
/supervise gate steading#22
/supervise hold
/supervise report
```

## Why this exists

The first week on stead was supervised by hand, from the conversation, and the same lessons were learned more than once:

- **A park gate completed into a live outage burns the next three attempts in under a minute.** The rule "wait for a real request to succeed" was found on the second gate, not the first.
- **A decision left as a node comment never reaches a session.** mill's prompt carries the node body, the results of blocking nodes, and failure feedback; not comments. Four review rounds on steading#6 were spent talking past the implementer before that was traced.
- **The reason a run died was in the database, not the journal**, three times. The queries are the same each time and were rediscovered each time.
- **The guest has its own habits**: `--socket` on every mill call, `--utc` on the journal, checkouts owned by the guest user, a bundle to reset them, no `strings` binary, a classifier that blocks `gh pr merge`.
- **Reporting drifted.** The person asked twice for the shape they wanted: the node's title on first mention, then a concise summary of what a stage did.

`docs/retrospectives/2026-09-22-first-week-on-stead.md` is the record this skill was written from. The runbook beside it holds the recipes so the next supervisor does not pay for them again.

## What it does NOT do

It does not complete a gate that asks for a person's judgment, hold or release nodes, stop the worker, or push anywhere without saying so in chat first. It does not read the person's vault. It never prints a token, and node bodies carry no hostnames.

## Files

- `SKILL.md` — the playbook the agent runs
- `RUNBOOK.md` — orientation, the monitor script, log reading, gate triage, checkout reset, filing shape, the cap, restarts
