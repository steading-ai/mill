package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/run"
)

// The `feature` workflow without the two steps that talk to a forge.
//
// `feature` opens the pull request before the reviewer reads the change, so
// a run on a host with no forge parks at `pr` and the reviewer never speaks.
// The seeded bad implementation is about what the reviewer says, so the
// scenario runs every step `feature` defines up to and including `review`,
// taken from `workflows/feature/workflow.yaml` itself rather than written
// out again here: the skills, the harness settings, the retries and the
// `on_failure: revise implement` that carries a no back to the implementer
// are the ones a delivering run uses, because they are the same bytes.
//
// What is dropped is exactly the steps whose evidence is a fact only a forge
// can state — `pr`, whose evidence is a URL, and the `merge` gate, whose
// evidence is a status. Nothing else differs, and the spec asserts the
// derivation kept `review` and dropped nothing else.

// forgelessFeature is the name the derived definition is pushed under. It is
// not `feature`: two definitions under one name are two versions of it, and
// a scenario running beside this one must keep selecting the real thing.
const forgelessFeature = "feature-no-forge"

// deriveForgelessFeature drops the two steps whose evidence only a forge
// can state and pushes what is left under a name of its own, into the
// scenario's own dagger project.
//
// The scope is the point. A derived definition is one scenario's scaffolding
// and not a workflow a deployment has, so it belongs where only that
// scenario can see it: an org definition is a candidate the assessment may
// choose for *every* project, and the suite's store outlives any one run of
// it, so a forgeless variant pushed org-wide is a workflow that every later
// assessment — in this suite run and in every one after it — gets to pick
// instead of `feature`.
func deriveForgelessFeature(ctx context.Context, project string) (dir string, dropped []string) {
	GinkgoHelper()
	definition := featureDefinition()
	definition["name"] = forgelessFeature
	definition["steps"] = keepSteps(definition, func(step map[string]any) bool {
		if talksToForge(step) {
			dropped = append(dropped, stepName(step))
			return false
		}
		return true
	})
	return stageWorkflow(ctx, definition, "--scope", "project", "--project", project), dropped
}

// deriveGatedFeature is `feature` with one thing changed: `plan-approval`
// has no `auto`, which the loader reads as "always ask" (internal/workflow,
// predicate.UnmarshalYAML). Every other byte is the delivering workflow's.
//
// It keeps the name `feature` and is pushed into the scenario's own dagger
// project, so a run there resolves `feature` to this definition and a run
// anywhere else still resolves it to the org one: a project definition
// shadows an org definition of the same name, which is the precedence both
// selection rules use (docs/domain-model.md §Selection, run.InScope). That
// is what lets the assessment name `feature` — the workflow O1's done-when
// is about — and still put the plan to a person.
func deriveGatedFeature(ctx context.Context, project string) string {
	GinkgoHelper()
	definition := featureDefinition()
	gated := false
	for _, entry := range definition["steps"].([]any) {
		step, ok := entry.(map[string]any)
		Expect(ok).To(BeTrue(), "every step is a mapping")
		if stepName(step) != planApprovalStep {
			continue
		}
		gate, ok := step["gate"].(map[string]any)
		Expect(ok).To(BeTrue(), "the %s step is a gate", planApprovalStep)
		Expect(gate).To(HaveKey("auto"), "the %s gate has an auto policy to remove", planApprovalStep)
		delete(gate, "auto")
		gated = true
	}
	Expect(gated).To(BeTrue(), "feature has a %s step", planApprovalStep)
	return stageWorkflow(ctx, definition, "--scope", "project", "--project", project)
}

// planApprovalStep is the gate O1 is about: the plan, put to a person.
const planApprovalStep = "plan-approval"

// featureDefinition is workflows/feature/workflow.yaml decoded, for a
// scenario that needs it in one respect different. It is read at run time
// rather than written out again here so that a derived definition cannot
// drift from the workflow it is derived from.
func featureDefinition() map[string]any {
	GinkgoHelper()
	content, err := os.ReadFile(filepath.Join(workflowDir("feature"), "workflow.yaml"))
	Expect(err).NotTo(HaveOccurred())
	var definition map[string]any
	Expect(yaml.Unmarshal(content, &definition)).To(Succeed())
	Expect(definition["steps"]).To(BeAssignableToTypeOf([]any{}), "the feature workflow has steps")
	return definition
}

// keepSteps is a definition's steps, less the ones a predicate rejects.
func keepSteps(definition map[string]any, keep func(step map[string]any) bool) []any {
	GinkgoHelper()
	steps, _ := definition["steps"].([]any)
	kept := make([]any, 0, len(steps))
	for _, entry := range steps {
		step, ok := entry.(map[string]any)
		Expect(ok).To(BeTrue(), "every step is a mapping")
		if keep(step) {
			kept = append(kept, step)
		}
	}
	return kept
}

// stageWorkflow writes a derived definition into a directory of the spec's
// own, pushes it to the suite's store, and returns the directory — which is
// also what `--workflow-dir` needs, for the `clean-tree` check's script.
//
// The directory is laid out as the loader reads one: the skills `feature`
// keeps beside itself and the scripts it runs are symlinked rather than
// copied, so nothing here can drift from the workflow it is derived from,
// and `quick-fix` sits beside it because `feature` borrows four skills from
// it by name (internal/workflow/load.go, skillDir).
func stageWorkflow(ctx context.Context, definition map[string]any, push ...string) string {
	GinkgoHelper()
	name, _ := definition["name"].(string)
	Expect(name).NotTo(BeEmpty(), "the derived definition has a name")

	source := workflowDir("feature")
	root := filepath.Join(GinkgoT().TempDir(), "workflows")
	dir := filepath.Join(root, name)
	Expect(os.MkdirAll(dir, 0o750)).To(Succeed())
	derived, err := yaml.Marshal(definition)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, "workflow.yaml"), derived, 0o600)).To(Succeed())
	for _, beside := range []struct{ from, to string }{
		{filepath.Join(source, "skills"), filepath.Join(dir, "skills")},
		{filepath.Join(source, "scripts"), filepath.Join(dir, "scripts")},
		{workflowDir("quick-fix"), filepath.Join(root, "quick-fix")},
	} {
		Expect(os.Symlink(beside.from, beside.to)).To(Succeed())
	}

	args := append([]string{"workflow", "push", dir, "--socket", suite.service.Socket}, push...)
	pushed := exec.CommandContext(ctx, env.Mill, args...) //nolint:gosec // the suite's own binary
	pushed.Stdout, pushed.Stderr = GinkgoWriter, GinkgoWriter
	Expect(pushed.Run()).To(Succeed(), "mill workflow push %s", name)
	return dir
}

// talksToForge reports whether a step's evidence is a fact only a forge can
// state: the URL of a pull request, or the status of a merge.
func talksToForge(step map[string]any) bool {
	switch step["evidence"] {
	case "url", "status":
		return true
	default:
		return false
	}
}

// stepName is a step's name, for the report of what was dropped.
func stepName(step map[string]any) string {
	name, _ := step["name"].(string)
	return name
}

// candidatesFor is the workflows a run on a project would be assessed
// between, by name: the same list `mill run` builds and hands the assessing
// session, computed by the same exported function (internal/run.InScope).
func candidatesFor(ctx context.Context, project string) []string {
	GinkgoHelper()
	res, err := suite.service.Store.ListWorkflows(ctx, connect.NewRequest(&millv1.ListWorkflowsRequest{}))
	Expect(err).NotTo(HaveOccurred())
	var names []string
	for _, workflow := range run.InScope(res.Msg.GetWorkflows(), project) {
		names = append(names, workflow.GetName())
	}
	return names
}

// expectCandidates refuses to spend a run on a store whose scope holds a
// workflow the scenario did not put there.
//
// The assessment picks one of these by name, so anything else in the list is
// something it may pick instead — and the suite's store is a deployment's,
// which outlives any one run of the suite and has no way to unpush. A
// leftover from an older suite run is therefore a real, silent cause of a
// run choosing the wrong workflow, and it costs a session to discover after
// the fact. Asserted before the run rather than after it, so the failure
// names the leftover instead of the choice it caused.
func expectCandidates(ctx context.Context, project string, want ...string) {
	GinkgoHelper()
	Expect(candidatesFor(ctx, project)).To(ConsistOf(want),
		"the assessment for %s chooses between exactly the workflows this suite pushed; "+
			"anything else is a leftover in %s that no `mill workflow` verb can remove — "+
			"see docs/runbook.md §\"Cleaning up\"", project, DatabaseURLVar)
}
