package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
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

// deriveForgelessFeature writes the derived workflow into a directory of the
// spec's own, pushes it to the suite's store, and returns the directory —
// which is also what `--workflow-dir` needs, for the `clean-tree` check's
// script.
//
// The directory is laid out as the loader reads one: the skills `feature`
// keeps beside itself and the scripts it runs are symlinked rather than
// copied, so nothing here can drift from the workflow it is derived from,
// and `quick-fix` sits beside it because `feature` borrows four skills from
// it by name (internal/workflow/load.go, skillDir).
func deriveForgelessFeature(ctx context.Context) (dir string, dropped []string) {
	GinkgoHelper()
	source := workflowDir("feature")
	content, err := os.ReadFile(filepath.Join(source, "workflow.yaml"))
	Expect(err).NotTo(HaveOccurred())

	var definition map[string]any
	Expect(yaml.Unmarshal(content, &definition)).To(Succeed())
	definition["name"] = forgelessFeature

	steps, ok := definition["steps"].([]any)
	Expect(ok).To(BeTrue(), "the feature workflow has steps")
	kept := make([]any, 0, len(steps))
	for _, entry := range steps {
		step, ok := entry.(map[string]any)
		Expect(ok).To(BeTrue(), "every step is a mapping")
		if talksToForge(step) {
			dropped = append(dropped, stepName(step))
			continue
		}
		kept = append(kept, step)
	}
	definition["steps"] = kept

	root := filepath.Join(GinkgoT().TempDir(), "workflows")
	dir = filepath.Join(root, forgelessFeature)
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

	push := exec.CommandContext(ctx, env.Mill, "workflow", "push", dir, "--socket", suite.service.Socket) //nolint:gosec // the suite's own binary
	push.Stdout, push.Stderr = GinkgoWriter, GinkgoWriter
	Expect(push.Run()).To(Succeed(), "mill workflow push %s", forgelessFeature)
	return dir, dropped
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
