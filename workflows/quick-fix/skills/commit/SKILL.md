---
name: commit
description: Commit the change on the run's branch with an intentional file selection and a conventional message that explains why. Produces the commit a step's evidence resolves.
---

# Commit

Commit the work. The commit SHA is what closes this step, so the commit has
to exist and be reachable on the run's branch.

You are running unattended. Nothing here waits for confirmation: stage,
write the message, and commit.

## Constraints

- **Never `git add` or `git commit` yourself.** The workspace commits, through
  its `git.commit` verb (below): it stages exactly the paths you name and
  commits on the run's branch as the mill, signed with the mill's key, which
  is on the host and never in this session. A commit you make yourself is
  unsigned, and the step that pushes refuses it.
- **Never set an identity or a signature, and never change git's
  configuration.** `--author`, `-c user.name=`, `-c user.email=`,
  `--no-gpg-sign` and `git config` are all wrong here, and a commit that
  carries any other identity is refused by the step that verifies this one.
- **Never amend, rebase, or force.** The run's history is the record of what
  happened; a rewritten commit makes earlier evidence point at nothing.
- **Never push.** The step that opens the pull request pushes.

## Instructions

### 1. See what changed

```bash
git status --porcelain
git diff
git diff --cached
git log --oneline -5
```

The last one is for the message style this repository actually uses; follow
it over the format below where they disagree.

### 2. Choose the files

Stage the files the change is made of, by path. Include:

- Files the previous step edited.
- Generated files the project commits, if the change regenerated them.
- `docs/plans/`, if a plan step wrote one: the plan belongs with the change it
  planned, not left as an uncommitted file the clean-tree check has to answer
  for.

Exclude, and report rather than commit:

- Anything unrelated to this node.
- Anything that looks like a credential: `.env`, keys, tokens.
- Scratch files and debug output. If any exist, delete them — a check after
  this step fails the run if the tree is not clean.

### 3. Write the message

Conventional Commits:

```
<type>(<scope>): <subject>

<body>
```

- **Types:** `feat`, `fix`, `chore`, `docs`, `refactor`, `test`, `style`.
- Subject in the present tense, ≤72 characters, no trailing period.
- The body says **why**. The diff already says what.
- Wrap the body at 80 columns.
- Name the node the change is for, so the commit points back at its work
  order.

### 4. Commit

The workspace's socket is at `$WORKSPACE_SOCKET` and `$STEP_TOKEN` is the
bearer it wants; every verb is one JSON `POST` to `http://mill/v1/<verb>`.
Write the request to a file — a message with backticks or quotes does not
survive a shell argument — naming the paths from step 2 and the message from
step 3:

```bash
cat > /tmp/commit.json <<'EOF'
{
  "message": "fix(scope): subject line\n\nWhy this change was made.\n\nRefs: <node ref>\n",
  "paths": ["path/one", "path/two"]
}
EOF
curl -sS --fail-with-body --unix-socket "$WORKSPACE_SOCKET" \
  -H "Authorization: Bearer $STEP_TOKEN" -H 'Content-Type: application/json' \
  --data @/tmp/commit.json http://mill/v1/git.commit
rm /tmp/commit.json
```

The response is the commit: `{"sha": "...", "subject": "...", ...}`. An error
body names what was refused — a path outside the tree, nothing staged — and
is yours to fix and retry.

### 5. Report the SHA

End with the full `sha` the verb returned on a line of its own (`git
rev-parse HEAD` shows the same). That is what this step is closed on; a
summary that does not contain it closes nothing.
