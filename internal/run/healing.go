package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/skift"
)

// greenCommentBody is the fixed message a claimed finding is told once its
// branch has gone green. Fixed rather than derived, so a later Reconcile
// call can tell "already told" from "not yet" by reading it back as
// [currentBody] does for a moved sha: skift fences a label change against
// whatever lease holds the node, which this reconciler never does, so
// [skift.Client.CommentFinding]'s comment-only write is the only one that
// can reach a claimed node at all.
const greenCommentBody = "main is green now."

// Healing keeps skift's picture of a red default branch honest: one open
// bug node per repository, filed when its default branch's checks first
// fail, kept current as the red sha moves, and dropped — or, if someone has
// claimed it, quietly marked green — once the branch is green again
// (docs/architecture.md §"Failure and stop"). RedMainFindings is the probe:
// it reads forges and never touches skift. Reconcile is the seam a future
// webhook receiver plugs its own findings into without a poll loop of its
// own.

// Finding is one thing wrong with a repository's default branch,
// independent of how it was found: the shape both RedMainFindings and a
// later webhook receiver build.
type Finding struct {
	// Key identifies the condition, not the moment: the repository and its
	// ref, deliberately excluding the sha, which is state that changes
	// under an unchanged key rather than a new condition.
	Key string
	// Project is the skift project the finding's node belongs in.
	Project string
	// Title is the node's title, were the finding filed today.
	Title string
	// Body is the node's body, were the finding filed today — and what an
	// already-filed node's body is compared against to decide whether to
	// comment.
	Body string
	// Labels are added to a filed node beside HealingLabel and its KeyLabel.
	Labels []string
}

// HealAction is one write Reconcile actually made, so a caller can turn it
// into a ledger finding without re-deriving what happened from Reconcile's
// side effects.
type HealAction struct {
	// Key is the finding's own key; empty for a drop or a green comment,
	// which act on a node no current finding names.
	Key string
	// Ref is the node the action acted on.
	Ref string
	// Verb is "filed", "commented", "commented-green", or "dropped".
	Verb string
}

// Reconcile converges every project's open findings against the given
// list: a node is filed for a finding with none yet, commented when its
// body has moved on, and left alone when nothing has changed. A node this
// reconciler filed earlier under a key no finding carries now is dropped
// when nobody claimed it, or commented once that its condition cleared
// when someone did. It takes the full project list rather than only the
// projects findings mention, so a project that has gone entirely green
// this call is still visited to drop its stale node.
func Reconcile(ctx context.Context, base *skift.Client, projects []string, findings []Finding) ([]HealAction, error) {
	byProject := map[string][]Finding{}
	for _, finding := range findings {
		byProject[finding.Project] = append(byProject[finding.Project], finding)
	}

	var actions []HealAction
	for _, project := range projects {
		client := base.Scoped(project)
		nodes, err := client.Findings(ctx)
		if err != nil {
			return actions, fmt.Errorf("healing: listing findings in %s: %w", project, err)
		}
		byKey := map[string]*skiftv1.Node{}
		for _, node := range nodes {
			if label, ok := keyLabelOf(node.GetLabels()); ok {
				byKey[label] = node
			}
		}

		seen := map[string]bool{}
		for _, finding := range byProject[project] {
			label := skift.KeyLabel(finding.Key)
			seen[label] = true
			change := changeID(finding.Key, finding.Body)

			node, exists := byKey[label]
			if !exists {
				requestID := "mill-heal-create-" + label + "-" + change
				filed, err := client.FileFinding(ctx, requestID, skift.Finding{
					Title:  finding.Title,
					Body:   finding.Body,
					Labels: append([]string{label}, finding.Labels...),
				})
				if err != nil {
					return actions, fmt.Errorf("healing: filing %s: %w", finding.Key, err)
				}
				actions = append(actions, HealAction{Key: finding.Key, Ref: filed.GetReference(), Verb: "filed"})
				continue
			}
			current, err := currentBody(ctx, client, node)
			if err != nil {
				return actions, fmt.Errorf("healing: reading %s's current body: %w", node.GetReference(), err)
			}
			if current == finding.Body {
				continue
			}
			requestID := "mill-heal-comment-" + node.GetReference() + "-" + change
			if err := client.CommentFinding(ctx, requestID, node.GetReference(), finding.Body); err != nil {
				return actions, fmt.Errorf("healing: commenting %s: %w", node.GetReference(), err)
			}
			actions = append(actions, HealAction{Key: finding.Key, Ref: node.GetReference(), Verb: "commented"})
		}

		for label, node := range byKey {
			if seen[label] {
				continue
			}
			ref := node.GetReference()
			if !node.GetClaimed() {
				requestID := "mill-heal-drop-" + ref
				if err := client.DropFinding(ctx, requestID, ref, "the branch this finding named is green again"); err != nil {
					return actions, fmt.Errorf("healing: dropping %s: %w", ref, err)
				}
				actions = append(actions, HealAction{Ref: ref, Verb: "dropped"})
				continue
			}
			current, err := currentBody(ctx, client, node)
			if err != nil {
				return actions, fmt.Errorf("healing: reading %s's current body: %w", ref, err)
			}
			if current == greenCommentBody {
				continue
			}
			requestID := "mill-heal-green-" + ref
			if err := client.CommentFinding(ctx, requestID, ref, greenCommentBody); err != nil {
				return actions, fmt.Errorf("healing: commenting green on %s: %w", ref, err)
			}
			actions = append(actions, HealAction{Ref: ref, Verb: "commented-green"})
		}
	}
	return actions, nil
}

// currentBody is what a filed node has last been told, for comparison
// against a finding's current one: a node's own Body is fixed at creation
// (skift has no update-body op), so the answer once a finding has moved on
// at least once is its most recent comment, not the node's own field.
func currentBody(ctx context.Context, client *skift.Client, node *skiftv1.Node) (string, error) {
	comments, err := client.Comments(ctx, node.GetReference())
	if err != nil {
		return "", err
	}
	if len(comments) == 0 {
		return node.GetBody(), nil
	}
	return comments[len(comments)-1].GetBody(), nil
}

// keyLabelOf finds the one label [skift.KeyLabel] produced among a node's
// labels, so it can be matched back to the finding it was filed for without
// storing anything outside skift.
func keyLabelOf(labels []string) (string, bool) {
	for _, label := range labels {
		if strings.HasPrefix(label, skift.KeyLabelPrefix) {
			return label, true
		}
	}
	return "", false
}

// changeID fingerprints a finding's key and body for a request id that must
// tell two different incidents under the same key apart. Finding carries no
// sha of its own — Reconcile stays agnostic of what changed inside a key,
// so a webhook receiver's findings need no sha either — so the fingerprint
// is of the content itself rather than a parsed-out sha: a second incident
// under the same key, with a different body, mints a different id and is
// never refused as a same-id-different-content replay of the first.
func changeID(key, body string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + body))
	return hex.EncodeToString(sum[:])[:8]
}

// RedMainFindings probes every project's default branch for a failing
// check. It never touches skift — Reconcile (above) is the only thing that
// does — so the same findings it returns are exactly what a future webhook
// receiver would hand Reconcile instead.
//
// projectNamed and repositories are given as the two separate calls a
// caller already has to hand (Skift.ProjectNamed, Options.Repositories),
// matching Runner.repositoryOf's own two-call resolution
// (internal/run/run.go:709-720), rather than one closure that hides them.
// A failure at either, or reading the branch's head or its checks, warns
// and moves to the next project; it never produces a Finding and never
// stops the pass.
func RedMainFindings(
	ctx context.Context,
	projects []string,
	projectNamed func(ctx context.Context, slug string) (*skiftv1.Project, error),
	repositories Repositories,
	warn func(project string, err error),
) []Finding {
	var findings []Finding
	for _, slug := range projects {
		project, err := projectNamed(ctx, slug)
		if err != nil {
			warn(slug, err)
			continue
		}
		repo, err := repositories(ctx, project)
		if err != nil {
			warn(slug, err)
			continue
		}
		ref, sha, err := repo.Forge.DefaultHead(ctx)
		if err != nil {
			warn(slug, err)
			continue
		}
		status, err := repo.Forge.Checks(ctx, sha)
		if err != nil {
			warn(slug, err)
			continue
		}
		if status != forge.Failed {
			continue
		}
		checks, err := repo.Forge.FailingChecks(ctx, sha)
		if err != nil {
			warn(slug, err)
			continue
		}
		findings = append(findings, redMainFinding(slug, repositoryPath(slug, repo), ref, sha, checks))
	}
	return findings
}

// repositoryPath is the repository's own path, parsed from its
// credential-free clone URL (Repository.Origin carries none — see
// internal/cli/run.go's repositoryFor) so a finding's key and body name the
// repository itself and not the skift project it happens to be filed
// under. A URL an operator's `--origin` override left unparsable falls back
// to the project's slug rather than aborting the probe over it.
func repositoryPath(slug string, repo Repository) string {
	parsed, err := forge.ParseRepo(repo.Origin)
	if err != nil {
		return slug
	}
	return parsed.Path
}

// redMainFinding builds the Finding acceptance criterion 1 and 8 describe:
// titled with a 7-character sha and the first failing check's name, and a
// body naming the repository's path, its ref, the full sha, and every
// failing check by name and URL — never the worker's own host or any
// credential.
func redMainFinding(project, path, ref, sha string, checks []forge.CheckResult) Finding {
	name := "a check"
	if len(checks) > 0 {
		name = checks[0].Name
	}
	var body strings.Builder
	fmt.Fprintf(&body, "%s's %s is red at %s.\n\nFailing checks:\n", path, ref, sha)
	for _, check := range checks {
		if check.URL != "" {
			fmt.Fprintf(&body, "- %s (%s)\n", check.Name, check.URL)
		} else {
			fmt.Fprintf(&body, "- %s\n", check.Name)
		}
	}
	return Finding{
		Key:     "red-main:" + path + ":" + ref,
		Project: project,
		Title:   fmt.Sprintf("main is red at %s: %s", shortSHA(sha), name),
		Body:    body.String(),
	}
}

// shortSHA is a commit sha as a title names it: seven characters, or the
// whole thing when it is already shorter.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
