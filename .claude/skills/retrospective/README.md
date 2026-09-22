# `/retrospective` — The Week, From the Record, in Rounds

Writes `docs/retrospectives/<slug>.md`: what the factory built, what it cost in tokens and lines, how that compares to the week before, what went wrong and what is still missing. Then carries the document through numbered rounds as the person comments in `%%` blocks and the agent answers, corrects itself, and proposes the next iteration.

```
/retrospective first-week-on-stead
/retrospective o3 since 2026-09-27
/retrospective round 2 first-week-on-stead
```

## Why this exists

- **Memory is the wrong source.** The first week's write-up began from what the supervisor remembered and was corrected twice by the ledger: last week was not all Opus, and this week did not produce less. Every number in the document has to name where it came from, and the recipes for getting them are longer than anyone will retype.
- **A claim without a trace cannot be argued with.** "Comments after approval were half-followed" read well and was wrong; the event log showed no session had read them at all. The person asked for the citation, and the correction was the most useful paragraph in the document. Rounds make that the normal path rather than an embarrassment.
- **The comments are the point.** The person writes into the document between `%%` markers; the agent appends a round underneath and leaves those blocks byte-for-byte. Restructuring or rewording a comment loses the record of what was actually said.
- **A list of everything wrong is not a plan.** Round 2 ends with the few items the record says would remove most of the cost, mapped to buckets and sized for a node, with the rest deferred and the reason stated. That is what the person can say yes or no to.

The first document written this way is `docs/retrospectives/first-week-on-stead.md`; `o2.md` beside it is the milestone shape it descends from.

## What it does NOT do

It does not commit or push; the person commits when a round is done. It does not rewrite earlier rounds except to append corrections in the new one, and it does not touch a `%%` block.

## Files

- `SKILL.md` — the playbook
- `QUERIES.md` — the data recipes: pull requests and lines by class, nodes, `mill cost` sections and the funnel, ledger waste counts, the server's ledger for the week before, `ccstat` for the supervisor's own session
