---
name: pr
description: Push the run's branch and open the pull request for it, then report the URL. The step that puts the work somewhere a human and the forge can both see it.
---

# Pull Request

Push the branch and open the pull request. The URL the forge returns is what
closes this step.

You are running unattended. Every question this skill would once have asked
has a default; take it and note it in the PR body.

| Question | Default |
|---|---|
| Which base branch? | The repository's default branch |
| Which template, when several exist? | The first in the list below that exists |
| The diff is empty — still open a PR? | No. Report that there is nothing to open a PR for and stop |
| Uncommitted changes are present? | Do not commit them. Report it and stop; a check ahead of this step already guards it |
| Draft or ready? | Ready |

## Instructions

### 1. Gather what the PR is made of

```bash
git rev-parse --abbrev-ref HEAD
git log --oneline origin/HEAD..HEAD
git diff --stat origin/HEAD..HEAD
```

### 2. Find the template

The first of these that exists:

```
.github/pull_request_template.md
.github/PULL_REQUEST_TEMPLATE/*.md
pull_request_template.md
PULL_REQUEST_TEMPLATE/*.md
docs/pull_request_template.md
docs/PULL_REQUEST_TEMPLATE/*.md
```

Fill it in. If none exists, use:

```markdown
## Why

Problem and motivation, in one to three sentences. Name the node.

## What

The approach, briefly. Not a walkthrough of the diff.

## Notes for reviewers

Decisions that are not obvious from the diff, anything uncertain, and every
default this run took where a human would have been asked.
```

### 3. Title

Derive it from the commits. Use the conventional-commit form if the
repository does. Keep it under 72 characters.

### 4. Push and open

The push and the pull request are the workspace's, through its `git.push`
and `forge.pr` verbs: the forge credential is on the host and never in this
session, so `git push` and `gh` here have nothing to present and fail. The
workspace's socket is at `$WORKSPACE_SOCKET` and `$STEP_TOKEN` is the bearer
it wants; every verb is one JSON `POST` to `http://mill/v1/<verb>`.

Push the run's branch:

```bash
curl -sS --fail-with-body --unix-socket "$WORKSPACE_SOCKET" \
  -H "Authorization: Bearer $STEP_TOKEN" -H 'Content-Type: application/json' \
  --data '{}' http://mill/v1/git.push
```

A push refused because a commit is not signed by the mill is a commit made
outside the `commit` step's verb; report it and stop.

Write the request to a file rather than passing it inline — backticks and
code fences do not survive a shell argument — and open the pull request
against the default base (`target` empty):

```bash
cat > /tmp/pr.json <<'PRBODY'
{"title": "...", "description": "...", "target": ""}
PRBODY
curl -sS --fail-with-body --unix-socket "$WORKSPACE_SOCKET" \
  -H "Authorization: Bearer $STEP_TOKEN" -H 'Content-Type: application/json' \
  --data @/tmp/pr.json http://mill/v1/forge.pr
rm /tmp/pr.json
```

Write the files under `/tmp`, never in the tree: it must stay clean.

### 5. Report the URL

The response is the pull request: `{"number": ..., "url": "...", ...}`. End
with its `url` on a line of its own. That is the evidence this step is
closed on.

If a pull request for this branch already exists — a resumed run reaches
this step twice — `forge.pr` returns it rather than opening a second one;
report its URL the same way.
