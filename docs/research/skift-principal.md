# How dagger treats an agent running under the owner's uid

Repo: `/Users/ivy/src/github.com/ivy/dagger` at `8cb1287`. Short version: mill-as-user is not a hole, it is the documented delegation mechanism, and `ReportUsage` is written on the assumption that the orchestrator *is* the user principal. Dagger will not stop mill from doing anything with gates; attribution of "mill vs. hand" rests on the lease, not the principal.

## 1. What the policy does for a `user` principal

`policy/dagger.rego` grants a user every action class outright, then applies vetoes. Every gate veto except one is scoped `input.principal.kind != "user"`.

- **(a) claim a node, including a gate.** Permitted: `permitted if { input.principal.kind == "user"; input.action == "claim_node" }` (`policy/dagger.rego:90-93`). The only claim veto is agent-scoped: `input.action == "claim_node"; input.principal.kind == "agent"; gate(input.resource.node)` (`:296-301`). A user claiming a gate is allowed.
- **(b) file a gate.** `create_node` is in `node_actions`, permitted for users (`:52`, `:70-73`). No veto touches creation.
- **(c) complete a gate it filed.** Two vetoes could apply; both exclude users. `complete_node ... gate(...) ... input.principal.kind != "user"` (`:241-246`) and `input.action in {"drop_node","complete_node"}; filed_gate(...); input.principal.kind != "user"` (`:260-265`). Allowed. Test `test_user_filer_disposes_of_its_own_gate` asserts exactly this (`policy/dagger_test.rego:212-217`).
- **(d) drop / reassign / unlink a gate it filed.** Drop: same rule as (c). Reclassify: `update_node; filed_gate; input.principal.kind != "user"; some field in {"assignee","assignee_kind","kind"}` (`:267-274`). Unlink: `remove_edge; filed_gate(end); input.principal.kind != "user"` (`:276-282`). All allowed for a user.
- **(e) report usage.** `permitted if { input.principal.kind == "user"; input.action in usage_actions }` (`:85-88`, `usage_actions` at `:60`). Test `test_user_records_usage_anywhere` (`dagger_test.rego:350-352`). The Go layer adds a second check, see §4.

Nothing in (a)–(e) is refused for a user principal. The only vetoes a user still meets are the non-gate ones: editing another author's comment (`:287-291`) and lease fencing (`heartbeat_lease`/`release_lease` need `holder == principal`, `:223-226`).

## 2. The reasoning dagger recorded

ADR-009 (`docs/adrs/009-the-default-gate-policy-is-notification.md:12`): "An agent working interactively under a user's uid *is* that user principal. The filer veto therefore refused the ordinary interactive handoff ... because the policy cannot tell the user from the agent that shares the uid. In a one-person organization that is a deadlock rather than a safeguard." Decision (`:31`): "The default is a notification channel, not a safety boundary ... It does not attempt to separate two humans from each other." Net effect (`:45`): "a user principal has full control of the gates it files. An agent principal cannot complete, claim, reclassify, or unlink any gate, whether or not it filed it."

ADR-006 §"A trust zone is the delegation mechanism" (`docs/adrs/006-provenance-not-authority.md:62-68`): "Every process one unix user runs is therefore one principal. An agent working interactively under a user's uid **is** that user principal, with the user's authority over the whole organization; running it there is the grant, and killing it is the revocation. Its writes are `human_touched` because the user vouches for them." And: "An agent that shares a uid with a person is inside that person's zone by construction, and that is a deployment decision, not a policy bug."

`docs/domain-model.md:258` repeats it under **A user disposes of its own gates**. The vault note (`2026-09-06 - Gate policy and trust zones.md:13-16`) is the same model in her words: "That is the delegation mechanism, not a gap."

Verdict: **intended**, not tolerated. The docs describe the *interactive* agent; nothing distinguishes an unattended loop under the same uid, and ADR-009 names the headless case as "an agent in its own trust zone with its own `agent` principal" (`:20`). Mill running unattended under her uid is a case the docs do not name separately; the trust-zone text covers it by construction ("every process one unix user runs").

## 3. Provenance when mill writes as the user

`Event` fields (`proto/dagger/v1/dagger.proto:443-475`, `docs/domain-model.md:318-335`):

| Field | Distinguishes mill from hand? |
|---|---|
| `principal`, `principal_kind` | No. Both are her `user` principal. |
| `identity_scheme` | No, while both reach the socket (`unix`). It would separate laptop-over-tailnet (`tailscale`) from server-side work once tsnet is enabled, which is a transport distinction, not a mill distinction. |
| `on_behalf_of` | No. **Reserved only.** Output-only in proto (`dagger.proto:237-240`, `:457-459`), column exists (`internal/store/queries/events.sql:2`), wired to the wire type (`internal/server/wire.go:341,362`), but no domain code ever sets it: the only references are the sqlc insert and the read-side deref. Domain-model §Delegation: "It is **not in v1**" (`docs/domain-model.md:178-180`). |
| `request_id` | Yes, by mill's own convention. It is the client-supplied idempotency key (`internal/domain/patch.go:48-49`). A recognisable prefix makes every mill patch findable via `ListEventsForRequest` (`events.sql:22-26`). Not enforced. |

`Lease` fields (`dagger.proto:228-258`): `harness`, `agent_ref`, `session_ref` are client-supplied on `ClaimNext` (`service.proto:733-740`), stored on the lease (`internal/domain/lease.go:270-272`), and the `claim_node` event's `after` is the whole lease row (`lease.go:275`, `encodeChange` marshals it at `internal/domain/events.go:103-107`; `store.Lease` carries `Harness`, `AgentRef`, `SessionRef` at `internal/store/models.gen.go:101-114`). So **every claim mill makes carries `harness=mill`** in the event log, and every fenced write after it carries the lease id in the patch. `Usage` rows carry `lease_id` (`dagger.proto:386-388`).

"Show me everything mill did today": no first-class query. `WatchEvents` filters by project and op only (`service.proto:829-840`; `dg events --op`, `internal/cli/events.go:65-66`); the stored queries are by node, request, or position (`events.sql`). Lease queries are per node or per holder (`internal/store/queries/leases.sql:16,21`), and holder is her principal. So the answer is a join a consumer does: `claim_node` events where `after.harness == "mill"` give the lease ids; events under those leases, or with mill's request-id prefix, are mill's. Non-lease writes mill makes (filing a gate with no lease, adding edges to filed work) are only distinguishable by `request_id` convention.

`human_touched` (`dagger.proto:207-210`, `events.sql:27-33`) becomes true on every node mill writes, because `principal_kind = 'user'`. ADR-006 calls that vouching.

## 4. What dagger expects an orchestrator's principal to be

The proto does not require `service`. `PrincipalKind` comments (`dagger.proto:63-75`): `AGENT` is "a model-driven agent or the orchestrator running one"; `SERVICE` is "a non-interactive service such as a notifier or an indexer." The only service principal that exists in code is the reaper (`internal/domain/reap.go:25,117`).

`ReportUsage`'s refusal rules, precisely:

- Proto: "It is the orchestrator's call: the process that runs an agent is the one that can count its tokens, and an agent principal is refused" (`service.proto:85-89`). The lease "must be the node's current lease and the caller's own" (`:806-810`).
- Server: `onlyOrchestrators` (`internal/server/work.go:150-165`): "**Only a user principal passes today: an orchestrator runs under the person whose work it dispatches, and no principal kind names an orchestrator.**" Code: `if actor.PrincipalKind == "user" { return nil }` else `PermissionDenied`. So **agent refused, service refused, user allowed** at the RPC door.
- Rego: user permitted anywhere (`dagger.rego:85-88`); agent permitted only on `own(node)` with its lease (`:163-167`); service has no permitting rule at all (`test_holder_does_not_record_usage_elsewhere`, `dagger_test.rego:365`). The Go check runs first, so the agent path in Rego is currently unreachable through `ReportUsage`.
- Domain: the report needs the carried lease, on that node, fenced (`internal/domain/usage.go:200-212`).

So dagger's current position is stronger than "tolerated": mill-as-user is the *only* principal kind that can call `ReportUsage` today. Note the tension with the roadmap's M2 wording, "a workflow runner that owns its own principal" (`docs/roadmap.md:29`), and the vault note's headless model (own uid, kind `agent`): under the code as written, a mill with its own `agent` principal could not report usage without `onlyOrchestrators` changing.

## 5. Consequences for mill's spec

- **"mill must refuse to claim or dispose of gates by its own discipline, since dagger will not stop it as a user principal."** True. Every gate veto is scoped away from users (§1). The `README.md:26-31` and `plugin/dagger/skills/dagger/SKILL.md:105-125` guidance ("policy refuses an agent principal that claims one") describes what happens to an `agent` principal; for mill it is only advice. Mill's own rule should be the doc's model: prepare in a node the gate depends on, never claim or complete the gate.
- **"mill's actions are attributable to mill via lease.harness/agent_ref even though the principal is the owner."** True for leased work, provided mill sets `harness` (and `agent_ref`/`session_ref`) on every `ClaimNext`. False for lease-less writes (filing a gate, adding an edge to filed work): those carry only her principal plus whatever `request_id` mill chooses. Mill should adopt a request-id prefix to close that gap.
- **"a gate mill files and the owner answers from Slack is the same principal completing it, and that is fine by design."** True; it is literally the scenario ADR-009 fixes (`:12`, `:49`). Two caveats: the event log cannot show that a person, rather than mill, completed it, so "fine" rests on mill's discipline not to complete gates; and if the Slack path lands on the server under her uid it is indistinguishable, while a laptop path over tsnet would show `identity_scheme = tailscale`.
- **"when mill later moves to its own account, only the identity row changes."** False as stated. Moving mill to its own uid means a *new principal of kind `agent`* (a `principals` row plus a `principal_identities` row, `internal/store/queries/principals.sql:1-24`), not a re-pointed identity row. With that, three things change: every gate veto starts applying to mill (correct), mill's writes stop being `human_touched`, and **`ReportUsage` refuses it** (`work.go:157-164`) until dagger grows an orchestrator principal kind or a delegation grant. Mill's non-own edges and comments also fall under agent scoping (`dagger.rego:130-215`).

## 6. What is still open

- **Stored delegation grants** are "Deliberately unscheduled" (`docs/roadmap.md:53`): "Worth building when a deployment needs an agent to hold a user's authority without running inside the user's zone." That is exactly the headless-mill case. `on_behalf_of` waits on it.
- **No principal kind names an orchestrator** (`work.go:156`). ADR-009's closing line: "Revisit when a deployment has agents that are neither a user's own processes nor their own trust zone" (`:72`). The roadmap M2 ("owns its own principal", `:29`) and `onlyOrchestrators` (user only) disagree today.
- **Ephemeral sandbox uids**: `identity.go` refuses an unmapped uid (`docs/roadmap.md:54`; vault note "Deferred: ephemeral identities", leaning gid mapping).
- **`dg admin` bootstrap** (plan node 1, `docs/plans/2026-09-prove-the-bet.md:44`) is open; principals are still seeded by homestead's templated SQL, so creating a second principal for mill is a `psql` job until it lands.
- **Gate end to end** (plan node 12, `:55`) and the notifier (node 11) are open: no gate has yet reached a person through a notifier, and `dagger#13` was the live proof the vault note expected.
- AGENT_REVIEW §8's remaining unaddressed suggestion is the grant itself (`AGENT_REVIEW.md:346-360`); her margin note there says "I'd punt on grants." The other two suggestions (claim-to-prepare guidance, teaching vetoes) shipped in `f48b9d5`.
