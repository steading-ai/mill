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
// spec that stages it — O1's seeded-bad-implementation test is a fixture
// whose repository already carries the wrong code, and nothing about the
// staging below changes for it.
type fixture struct {
	// Name distinguishes the scratch project and the dagger project this
	// fixture stages.
	Name string
	// Files seed the repository, path to content.
	Files map[string]string
	// Title and Body are the node as an agent receives it.
	Title string
	Body  string
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

// staged is a fixture set up and ready to run: the repository on the forge,
// the project and node in dagger, and a workspace root of the spec's own.
type staged struct {
	Project      *scratchProject
	Graph        *scratchGraph
	NodeRef      string
	WorkspaceDir string
}

// stage creates everything one scenario needs and registers its teardown.
func (f fixture) stage(ctx context.Context) *staged {
	GinkgoHelper()

	By("given a scratch project seeded with the fixture's files")
	project := newScratchProject(ctx, f.Name, f.Files)

	By("and a dagger project whose repository is that scratch project")
	graph := newScratchGraph(ctx, f.Name)

	By("and the fixture's node filed in it")
	nodeRef := graph.file(ctx, f.Title, f.Body)

	// The workspace root is the spec's own so it can count the run
	// directories under it, and so the mill's signing key and its
	// allowed_signers line are somewhere the spec can read them.
	workspaceDir := filepath.Join(GinkgoT().TempDir(), "workspaces")
	Expect(os.MkdirAll(workspaceDir, 0o700)).To(Succeed())

	return &staged{Project: project, Graph: graph, NodeRef: nodeRef, WorkspaceDir: workspaceDir}
}

// start invokes `mill run` for the staged node.
func (s *staged) start(ctx context.Context, opts runOptions) *worker {
	GinkgoHelper()
	return start(ctx, s.Graph, s.Project, s.NodeRef, s.WorkspaceDir, opts)
}
