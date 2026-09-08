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

```bash
git push -u origin <branch>
```

Write the body to a file rather than passing it inline — backticks and code
fences do not survive a shell argument:

```bash
cat > pr-body.md <<'PRBODY'
...
PRBODY
gh pr create --title "..." --body-file pr-body.md
```

Delete `pr-body.md` afterwards. The tree must stay clean.

### 5. Report the URL

End with the pull request URL on a line of its own. That is the evidence
this step is closed on.

If a pull request for this branch already exists — a resumed run reaches
this step twice — do not open a second one. Query it and report its URL:

```bash
gh pr view --json url --jq .url
```
