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

- **Never `git add -A` or `git add .`** Be intentional about every path.
- **Never `git -C <path>`** Run git from the working directory.
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

```bash
git commit -m "$(cat <<'EOF'
fix(scope): subject line

Why this change was made.

Refs: <node ref>
EOF
)"
```

### 5. Report the SHA

Run `git rev-parse HEAD` and end with the full SHA on a line of its own.
That is what this step is closed on; a summary that does not contain it
closes nothing.
