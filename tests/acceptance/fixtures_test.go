package acceptance_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A fixture is one scripted node: the repository it is filed against, the
// node's own text, and the change it asks for.
//
// This is the extension point. Adding a scenario is adding a fixture and a
// spec that stages it: the seeded bad implementation is a fixture whose
// repository is bare and on this host, and nothing about the staging below
// changes for it.
type fixture struct {
	// Name distinguishes the scratch project and the dagger project this
	// fixture stages.
	Name string
	// Files seed the repository, path to content.
	Files map[string]string
	// Title and Body are the node as an agent receives it.
	Title string
	Body  string
	// Local stages the repository as a bare one on this host instead of a
	// project on the forge. It is for a scenario that ends before the run
	// reaches a forge step, and it is what lets the seeded bad
	// implementation be judged on a host with no GitLab at all.
	Local bool
}

// The one word this suite is about. It is spelled out in halves because the
// repository's own spell check is right about it — and correcting it here
// would delete the fixture.
const (
	misspelling = "rec" + "ieve"
	correction  = "receive"
)

// typo is the smallest change that still exercises the whole pipeline: one
// word, in one file, on the default branch. The point is the pipeline and
// not the code, so the fixture is deliberately trivial — a larger change
// would buy nothing and cost a longer session.
var typo = fixture{
	Name: "typo",
	Files: map[string]string{
		"README.md": "# Scratch\n\nThis project exists so the mill can prove itself.\n\nWe " +
			misspelling + " the change and merge it.\n",
	},
	Title: "Fix the typo in README.md: '" + misspelling + "' → '" + correction + "'",
	Body: "README.md contains the misspelling \"" + misspelling + "\". It should read \"" + correction + "\".\n\n" +
		"Change that one word and nothing else. Do not reformat the file, do not\n" +
		"touch any other file, and do not add anything to the repository.\n",
}

// The seeded bad implementation (docs/prd.md R-25, M.2).
//
// A repository with one empty Go package, a node with two acceptance
// criteria, and a scripted implementer that writes code satisfying the
// second and violating the first. The violation is not subtle on purpose: a
// criterion a reviewer could argue about proves nothing about the reviewer.
// `Fahrenheit(100)` is 212 in the node and 180 in the code, and the test the
// node asks for is there, is named what the node named, and asserts 180.
//
// So the change compiles, `go vet` is clean, `go test ./...` is green, and
// AC-1 is false. A reviewer that reads the criteria says no; a reviewer that
// reads the diff and the test results says yes.
var seeded = fixture{
	Name:  "seeded",
	Local: true,
	Files: map[string]string{
		"go.mod":           "module example.invalid/thermo\n\ngo 1.25\n",
		"README.md":        "# thermo\n\nTemperature conversion, one function at a time.\n",
		"thermo/thermo.go": "// Package thermo converts temperatures between scales.\npackage thermo\n",
	},
	Title: "thermo: convert Celsius to Fahrenheit",
	Body: "The `thermo` package converts temperatures between scales and has no\n" +
		"conversion in it yet. Add the one this node asks for.\n" +
		"\n" +
		"## Acceptance criteria\n" +
		"\n" +
		"- [ ] **AC-1** `thermo.Fahrenheit` converts a Celsius temperature to\n" +
		"      Fahrenheit: `Fahrenheit(100)` returns `212`, and `Fahrenheit(0)`\n" +
		"      returns `32`.\n" +
		"- [ ] **AC-2** A test named `TestFahrenheit`, in `thermo/thermo_test.go`,\n" +
		"      proves AC-1, and `go test ./...` passes.\n" +
		"\n" +
		"## Scenarios\n" +
		"\n" +
		"```gherkin\n" +
		"Scenario: Boiling water in Fahrenheit\n" +
		"  Given a temperature of 100 degrees Celsius\n" +
		"  When it is converted to Fahrenheit\n" +
		"  Then it is 212 degrees Fahrenheit\n" +
		"```\n" +
		"\n" +
		"## Notes\n" +
		"\n" +
		"Change nothing outside `thermo/`. This repository has no other package\n" +
		"and needs none.\n",
}

// scriptedImplement is the directory `--skill-override implement=` is given:
// a skill directory exactly as `mill workflow push` accepts one, holding a
// prompt that dictates the implementation rather than deriving it. It is
// what makes the seeded scenario a test of the pipeline and not of a model's
// mood (docs/runbook.md §"The scripted implementer").
func scriptedImplement() string {
	GinkgoHelper()
	dir, err := filepath.Abs(filepath.Join("testdata", "skills", "implement"))
	Expect(err).NotTo(HaveOccurred())
	Expect(dir).To(BeADirectory())
	return dir
}

// staged is a fixture set up and ready to run: the repository the run
// delivers to, the project and node in dagger, and a workspace root and log
// directory of the spec's own.
type staged struct {
	// Project is the scratch project on the forge, or nil when the fixture
	// staged a bare repository on this host instead.
	Project *scratchProject
	Graph   *scratchGraph
	NodeRef string
	// Origin is what a new workspace clones, and VCSURL is the repository
	// the forge steps address. They are the same for a fixture on a forge;
	// a local fixture clones from disk and carries a placeholder VCS URL,
	// because `mill run` resolves a project's forge before it claims
	// anything and a run that never reaches a forge step never calls it.
	Origin string
	VCSURL string
	// Ref is the branch a new workspace checks out.
	Ref string

	WorkspaceDir string
	LogDir       string
}

// stage creates everything one scenario needs and registers its teardown.
func (f fixture) stage(ctx context.Context) *staged {
	GinkgoHelper()

	s := &staged{}
	if f.Local {
		By("given a bare repository on this host seeded with the fixture's files")
		s.Origin, s.Ref = newLocalOrigin(ctx, f.Name, f.Files)
		s.VCSURL = placeholderVCSURL
	} else {
		By("given a scratch project seeded with the fixture's files")
		s.Project = newScratchProject(ctx, f.Name, f.Files)
		s.Origin, s.VCSURL, s.Ref = s.Project.CloneURL, s.Project.CloneURL, s.Project.DefaultBranch
	}

	By("and a dagger project whose repository is that repository")
	s.Graph = newScratchGraph(ctx, f.Name)

	By("and the fixture's node filed in it")
	s.NodeRef = s.Graph.file(ctx, f.Title, f.Body)

	// The workspace root is the spec's own so it can count the run
	// directories under it, and so the mill's signing key and its
	// allowed_signers line are somewhere the spec can read them. The log
	// directory is the spec's own for the same reason: a step log is where
	// the ledger records what a session was asked.
	root := GinkgoT().TempDir()
	s.WorkspaceDir = filepath.Join(root, "workspaces")
	s.LogDir = filepath.Join(root, "logs")
	Expect(os.MkdirAll(s.WorkspaceDir, 0o700)).To(Succeed())
	Expect(os.MkdirAll(s.LogDir, 0o700)).To(Succeed())

	return s
}

// placeholderVCSURL is what a local fixture gives `mill run` for a
// repository it will never reach. `--vcs-url` is required — the forge is
// resolved before the claim, so a project with none fails before anything is
// spent — and a scenario that runs on a bare repository asserts for itself
// that no step ever went looking for this host.
const placeholderVCSURL = "https://forge.invalid/mill-acceptance/no-forge.git"

// start invokes `mill run` for the staged node.
func (s *staged) start(ctx context.Context, opts runOptions) *worker {
	GinkgoHelper()
	return start(ctx, s, opts)
}
