# mill plugin

Registers the mill MCP server and ships a skill teaching a session to read a
run through it, or through the `mill` CLI.

## What it installs

| Component | Path | Effect |
|-----------|------|--------|
| MCP server | `.mcp.json` | a `mill` server, run as `mill mcp` over stdio |
| Skill | `skills/mill/SKILL.md` | `mill:mill`, model-invoked when the conversation touches a mill run, `mill.toml`, or how a session in a mill machine reaches out |

The same skill rides with the worker: a `mill` running the machine backend
embeds this directory and writes it, with a `VERSION` breadcrumb beside
`SKILL.md`, to its skill directory, which every machine mounts read-only at
`~/.claude/skills/mill` (`docs/sandbox.md` §"Skill mount"). A session inside
a machine reads it without installing this plugin.

## Requirements

`mill` must be on `PATH` and able to reach a running service. The plugin does
not install or build it — see the repository root for that. If `mill` is
missing, the MCP server fails to start and the skill still loads, teaching the
CLI for a binary that is not there.

Check before installing:

```sh
mill version && mill doctor
```

`mill` finds the service through `--socket`, then `$MILL_SOCKET`, then
`$XDG_RUNTIME_DIR/mill/mill.sock`, then `/run/mill/mill.sock`. The plugin
passes no `--socket`, so a session reaching a service on a non-default path
inherits it from `MILL_SOCKET` in the environment the session was started
with.

What the session may do is decided by the unix account it runs as: the store
maps that account's uid to a role, and a `reader` is refused `rerun` and
`workspace_clean`. A uid the map does not name is refused as unauthenticated,
which is an administrative fix on the service, not a client one.

## Install

From this repository:

```sh
claude plugin marketplace add ./plugin
claude plugin install mill@mill
```

Then restart the session, or start a new one, so the MCP server is picked up.

Verify:

```sh
claude plugin list             # mill, enabled
claude plugin details mill     # components and their token cost
```

In a session, `/mcp` lists the `mill` server and its thirteen tools. `mill mcp
--describe` prints the same list as JSON without connecting to anything.

## Developing

Load the plugin straight from the working tree, with no marketplace and
nothing installed:

```sh
claude --plugin-dir ./plugin/mill
```

A plugin loaded this way takes precedence over an installed one of the same
name for that session.

Validate the manifests, the skill's frontmatter, and the structure:

```sh
claude plugin validate ./plugin              # the marketplace
claude plugin validate ./plugin/mill         # the plugin
claude plugin validate ./plugin --strict     # warnings are errors; for CI
```

Installing copies the plugin into
`~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/`, so editing the
source afterwards does not update the installed copy. Bump `version` in
**both** `.claude-plugin/plugin.json` and the marketplace's entry to publish a
change, or work from `--plugin-dir`, which reads the tree directly.

## Uninstall

```sh
claude plugin uninstall mill
claude plugin marketplace remove mill
```
