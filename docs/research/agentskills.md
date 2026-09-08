# Agent Skills spec — fit for "workflow = skill + metadata"

Source: `/Users/ivy/src/github.com/agentskills/agentskills` at `69ef37e` (2026-08-09). Authoritative text is `docs/specification.mdx` (`AGENTS.md` line 12: "`docs/specification.mdx` is authoritative for format requirements. Explanatory documentation, examples, tests, and implementations do not add requirements"). There is no JSON Schema in the repo; the only machine-readable definition is the Python reference library `skills-ref/`, which its README calls "intended for demonstration purposes only."

The whole normative frontmatter surface is six fields (`docs/specification.mdx:25-32`):

| Field | Required | Constraints |
|---|---|---|
| `name` | Yes | ≤64 chars, `[a-z0-9-]`, must match parent directory name |
| `description` | Yes | ≤1024 chars; "Describes what the skill does and when to use it" |
| `license` | No | free text |
| `compatibility` | No | ≤500 chars free text: "intended product, system packages, network access, etc." |
| `metadata` | No | "a map from string keys to string values" |
| `allowed-tools` | No | "Space-separated string of pre-approved tools the skill may use. (Experimental)" |

The body is unconstrained: "There are no format restrictions" (`specification.mdx:178`).

## Structured output contract

**Not specified.** Nothing in the frontmatter or body conventions names an output schema. The nearest guidance is prose-level: `docs/skill-creation/best-practices.mdx` "Templates for output format" (line 187) recommends embedding a markdown template in `SKILL.md` or `assets/` "because agents pattern-match well against concrete structures," and `assets/` may hold "Data files (lookup tables, schemas)" (`specification.mdx:214`). That is a hint to the model, not a contract a harness enforces.

So the contract has to live in orchestrator metadata. You can *point* at it from the spec's `metadata` map (string values only — see below), e.g. `metadata: { orchestrator.output-schema: "assets/output.schema.json" }`, and ship the schema in `assets/`; enforcement is yours.

## Model / effort / harness preference

**Not specified** for model or effort. The only harness-adjacent fields:

- `compatibility` — free text, explicitly for "intended product" (`specification.mdx:128`); example `compatibility: Designed for Claude Code (or similar products)` (line 133). Not machine-readable.
- `allowed-tools` — the one harness-specific setting that leaked into the spec, marked experimental: "Support for this field may vary between agent implementations" (line 167). Example uses Claude Code syntax: `allowed-tools: Bash(git:*) Bash(jq:*) Read` (line 172). The spec does not define the pattern grammar.
- `metadata` — the sanctioned extension point: "Clients can use this to store additional properties not defined by the Agent Skills spec. We recommend making your key names reasonably unique to avoid accidental conflicts" (lines 151-152).

The client guide notes other clients invent frontmatter flags outside the spec (`docs/client-implementation/adding-skills-support.mdx:226`: "e.g., via a `disable-model-invocation` flag") and treats them as client business. There is no carve-out that says "loaders MUST ignore unknown top-level keys"; see next-to-last section for what the validator actually does.

## Composition, chaining, and "when to use"

**Not specified.** No field lets a skill declare `requires`, `after`, `next`, or "invoke skill X." A skill body can name another skill in prose, and the body may reference bundled files, but the spec only defines relative file references ("Keep file references one level deep from `SKILL.md`", `specification.mdx:237`).

"When to use" lives entirely in `description`: "Should describe both what the skill does and when to use it" (line 95). The docs are blunt that this is a model judgment, not a harness mechanism: "the description carries the entire burden of triggering" and "Most implementations rely on the model's own judgment as the activation mechanism, rather than implementing harness-side trigger matching or keyword detection" (`adding-skills-support.mdx`, "Model-driven activation"). The docs also warn triggering is probabilistic — they recommend eval suites of ~20 queries × 3 runs with a 0.5 trigger-rate threshold (`optimizing-descriptions.mdx:87-91`).

Implication for the router decision: a description-driven "pick the next skill" router is exactly the spec's intended use of `description`, but the spec gives you no determinism guarantees for it. Explicit ordering must be orchestrator metadata. The client guide does sanction the execution model you want: "Subagent delegation (optional)… the skill is run in a separate subagent session. The subagent receives the skill instructions, performs the task, and returns a summary" (`adding-skills-support.mdx`, Step 5).

## Versioning and provenance

**Not specified.** The spec has no `version` field; the only appearance is as an example inside `metadata` (`specification.mdx:51-54`, `metadata: { author: example-org, version: "1.0" }`). There is no spec-version identifier either — `git log -- docs/specification.mdx` shows 17 editorial commits and no version tags. The spec's positioning is that skills are "portable, version-controlled folders" (`docs/home.mdx:27`), i.e. git is the versioning.

Name collisions are handled by scope precedence, not version: "project-level skills override user-level skills… Log a warning when a collision occurs" (`adding-skills-support.mdx:83-87`). Pinning "exactly which skill version ran" is therefore yours: record a content hash of the skill directory (or the seeding repo's commit + path) per step.

## Frontmatter schema, reserved fields, extensions

Reserved: the six fields above. Everything else is reserved-by-omission. Empirically, using the reference validator on two scratch skills (`skills-ref validate`, run via a venv outside the repo):

- A top-level `x-orchestrator:` block **fails**: `Unexpected fields in frontmatter: x-orchestrator. Only ['allowed-tools', 'compatibility', 'description', 'license', 'metadata', 'name'] are allowed.` (`skills-ref/src/skills_ref/validator.py:15-22, 104-115`).
- Keys under `metadata` **pass**, but nested values are silently stringified: `metadata: { nested: { a: 1 } }` reads back as `"nested": "{'a': '1'}"` (`parser.py:61-62`: `{str(k): str(v) …}`).

Real clients are told to be lenient — "Warn on issues but still load the skill when possible" (`adding-skills-support.mdx:128-140`) — but that list only covers name/description problems; unknown-key handling is not specified. So: a top-level `x-orchestrator:` block will likely load in most clients and will fail strict validation; a flat, string-valued, namespaced `metadata` map is the only fully compliant extension surface, and it cannot hold structure.

## Resources, scripts, tools

Directories `scripts/`, `references/`, `assets/` are "recommendations for organizing common types of content" (`specification.mdx:189`), not declared anywhere — there is no manifest. Scripts are referenced by relative path in the body ("Run the extraction script: `scripts/extract.py`", lines 230-235) and executed by the model, not the harness: "executable code that agents can run… Supported languages depend on the agent implementation" (lines 193-198). The client guide has activation tools *list* resources but "should **not** eagerly read them."

The deterministic-check idea is well supported as a *convention*: `using-scripts.mdx` recommends "Use structured output… JSON," "Separate data from diagnostics: send structured data to stdout… diagnostics to stderr," and "Meaningful exit codes… document them in your `--help`"; `best-practices.mdx` "Validation loops" and "Plan-validate-execute" are literally do-work → run `scripts/validate.py` → loop. But nothing in the spec lets a skill say "this script is the acceptance gate." The orchestrator must name the check.

## Who implements it, and how faithfully

`docs/snippets/clients.jsx` lists 46 clients including Claude Code, Claude, "ChatGPT & Codex" (instructions at `developers.openai.com/codex/skills/`), Gemini CLI, Cursor, GitHub Copilot, VS Code, OpenCode, Goose, Amp, Kiro, Factory, OpenHands. The quickstart claims "The same skill works in any compatible agent, including Claude Code and OpenAI Codex" (`quickstart.mdx:13`).

Fidelity is not measured in the repo — it's a logo showcase with links, no conformance matrix. What *is* documented as varying: `allowed-tools` support ("may vary"), whether frontmatter is stripped on activation, activation mechanism (file-read vs tool vs slash command), scan locations (`.agents/skills/` convention plus client-specific dirs), and subagent delegation ("only supported by some clients"). Portable core: `name`, `description`, body, relative file refs.

## Recommendation

"A workflow step is a skill plus metadata" is sound, with one boundary drawn clearly: the **skill** is the portable, harness-agnostic part (name, description, body, `scripts/`, `assets/`), and the **orchestrator record** owns everything that is ordered, typed, enforced, or harness-specific — because the spec is explicitly designed to stay small (`AGENTS.md:5-7`) and will not grow those fields.

Store the orchestrator record *beside* the skill (e.g. `workflow.yaml` in the skill directory, or in your store keyed by skill name), not in frontmatter. Put a flat `metadata:` breadcrumb in `SKILL.md` for discoverability — `orchestrator.workflow: "true"`, `orchestrator.schema: "assets/output.schema.json"` — nothing structured. Avoid top-level `x-*` keys; they break `skills-ref validate` today and any strict loader tomorrow.

The orchestrator-specific record should carry, since the spec cannot:

1. **Output contract** — JSON Schema path + how the final message is extracted/validated.
2. **Ordering** — explicit `after`/`next`/`on-failure` edges. Use `description` for the *human-and-router* explanation, but do not let the router be the only sequencing mechanism.
3. **Deterministic check** — which `scripts/` entry is the gate, its exit-code semantics, expected stdout shape. Convention from `using-scripts.mdx` (JSON on stdout, diagnostics on stderr) is a fine wire contract.
4. **Harness translation table** — per harness: model, effort, permissions. Map to Claude Code `allowed-tools`/`model`; leave `allowed-tools` out of `SKILL.md` unless the skill is Claude-only, and put the intent in `compatibility` prose.
5. **Provenance** — source repo, commit, path, and a content hash of the skill directory recorded per step run. The spec has no version field; `metadata.version` is a convention you can add but must not trust.
6. **Scope/precedence** — org vs project layering; mirror the documented rule (project overrides user/org, warn on shadowing) so seeded skills behave the way native clients would.
