package run

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// Where a step's `artifact` evidence comes from.
//
// A skill step closes on what the session's last message said, which is the
// right default: the artifact is the session's own report and nothing else
// produced it. A step may instead name a file in the checkout
// (docs/domain-model.md §Step, `artifact.path`), and then that file's content
// is the artifact. `feature`'s plan step is the case the field exists for:
// the plan is written to `docs/plans/` so it is committed with the change and
// stays in the repository, and the same bytes are attached to the node so a
// person reads the plan next to the work and a gate can be answered from it.
//
// The path is a template, because a file named for the run cannot be written
// down in a definition. Mill renders it, tells the session where to write
// ([Runner.artifactBrief]), and reads it back through the workspace when the
// session ends. Nothing the session says about the file is consulted: a step
// whose file is missing does not close.

// artifactBody is what the mill attaches for a step closing on `artifact`
// evidence: the file the step names, or the session's last word when it
// names none.
func (r *Runner) artifactBody(ctx context.Context, s *state, step *millv1.Step, final string) (string, error) {
	path, err := r.artifactPath(s, step)
	if err != nil || path == "" {
		return final, err
	}
	if s.workspace == nil {
		return "", fmt.Errorf("step %s: no checkout to read %s from", step.GetName(), path)
	}
	content, err := s.workspace.Read(ctx, path)
	if err != nil {
		return "", fmt.Errorf("step %s: %s is the file this step closes on, and it could not be read: %w",
			step.GetName(), path, err)
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return "", fmt.Errorf("step %s: %s is the file this step closes on, and it is empty", step.GetName(), path)
	}
	return string(content), nil
}

// artifactPath is the file a step's artifact evidence is read from, rendered
// from the run. Empty when the step names none.
func (r *Runner) artifactPath(s *state, step *millv1.Step) (string, error) {
	source := step.GetArtifact().GetPath()
	if source == "" {
		return "", nil
	}
	path, err := render(source, r.templateData(s, step))
	if err != nil {
		return "", fmt.Errorf("step %s: artifact path: %w", step.GetName(), err)
	}
	return path, nil
}

// artifactBrief is what the mill tells a session about the file its step
// closes on. It is appended to the prompt after the skill and the node, in
// the mill's own words like a revise's feedback, because the path is a fact
// about this run that no skill can carry: the skill says what to write, this
// says where and under whose name.
func (r *Runner) artifactBrief(s *state, step *millv1.Step) (string, error) {
	path, err := r.artifactPath(s, step)
	if err != nil || path == "" {
		return "", err
	}
	data := r.templateData(s, step)
	var b strings.Builder
	b.WriteString("## The file this step closes on\n\n")
	fmt.Fprintf(&b, "Write it to `%s` in the checkout. When the session ends the mill reads that "+
		"file and attaches its content as this step's evidence; nothing you say here closes the step, "+
		"and a file that is missing or empty fails it.\n\n", path)
	b.WriteString("These are the mill's own facts about this run, for the file's frontmatter:\n\n")
	for _, fact := range []string{"run", "node", "generation", "workflow", "date"} {
		fmt.Fprintf(&b, "- %s: %v\n", fact, data[factKeys[fact]])
	}
	return b.String(), nil
}

// factKeys maps the name a written record uses for a run fact onto the
// template's own. The two differ because a template reads as Go and a
// frontmatter key reads as YAML.
var factKeys = map[string]string{
	"run":        "RunID",
	"node":       "NodeRef",
	"generation": "Generation",
	"workflow":   "Workflow",
	"date":       "Date",
}

// templateData is what a definition's templates — a gate's ask, an artifact
// step's path — are rendered against: the run, the node, and the step, in one
// vocabulary so that a workflow author learns it once.
func (r *Runner) templateData(s *state, step *millv1.Step) map[string]any {
	return map[string]any{
		"NodeRef":     s.node.GetReference(),
		"NodeTitle":   s.node.GetTitle(),
		"NodeSlug":    slug(s.node.GetReference()),
		"Step":        step.GetName(),
		"Branch":      s.run.GetBranch(),
		"PullRequest": s.pullRequestURL,
		"RunID":       s.run.GetId(),
		"Generation":  s.run.GetGeneration(),
		"Workflow":    s.workflow.GetName(),
		"Date":        r.options.Now().UTC().Format("2006-01-02"),
	}
}

// slug is a node reference as a filename: `mill#26` becomes `mill-26`. The
// reference rather than the title, because a title is edited and a file named
// for one would be written twice under two names.
func slug(reference string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(reference) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
