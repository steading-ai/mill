package claude

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/testsupport"
)

// liveVar turns on the spec that spends a real session against the account's
// own Claude Code login. It is off by default: `make test` must not depend on
// a credential or on the model being reachable.
const liveVar = "MILL_SIDECAR_LIVE"

var _ = Describe("The claude adapter against the real sidecar", Label("live"), func() {
	var entrypoint string

	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 to run the specs that spend a real session")
		}

		root, err := filepath.Abs(filepath.Join("..", "..", "..", "sidecar"))
		Expect(err).NotTo(HaveOccurred())
		build := exec.Command("pnpm", "run", "build")
		build.Dir = root
		build.Stdout, build.Stderr = GinkgoWriter, GinkgoWriter
		Expect(build.Run()).To(Succeed(), "the sidecar did not build")
		entrypoint = filepath.Join(root, "dist", "main.js")
	})

	It("streams the session's events before the terminal record, and returns what it billed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		DeferCleanup(cancel)
		log := &recorder{}
		adapter := New(WithEntrypoint("node", entrypoint))

		turn, err := adapter.Start(ctx, harness.Session{
			Timeout: 4 * time.Minute,
			ID:      newSessionID(),
			Cwd:     GinkgoT().TempDir(),
			Events:  log.sink(),
		}, harness.Prompt{Text: "Reply with the single word: ready. Use no tools."}, nil)
		Expect(err).NotTo(HaveOccurred())
		log.watch(turn)

		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)
		Expect(log.collected()).NotTo(BeEmpty(), "the turn streamed no events")
		Expect(log.afterTerminal()).To(BeZero(), "an event arrived after the terminal record")
		Expect(terminal.Usage).NotTo(BeEmpty(), "the adapter is the only component that sees the harness's numbers")
		for model, counts := range terminal.Usage {
			Expect(counts.Output).To(BeNumerically(">", 0), "model %s billed no output tokens", model)
		}
	})
	// The scenario the first end-to-end run failed on: the session's `git
	// add` was refused by the SDK's permission layer with "This command
	// requires approval", and nothing was there to answer. It runs under
	// both environment modes, because a commit needs more of the host than a
	// prompt does — git, a HOME to read ~/.gitconfig from — and strict mode
	// is the one that could take it away.
	for _, mode := range []EnvMode{EnvInherit, EnvStrict} {
		It("A commit step commits ("+string(mode)+")", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			DeferCleanup(cancel)

			By("given a workspace checkout with README.md modified and a run at the commit step")
			checkout := scratchCheckout()
			before := headOf(checkout)

			log := &recorder{}
			adapter := New(WithEntrypoint("node", entrypoint), WithEnvMode(mode))

			By("when the commit skill's session runs under the default permission mode")
			turn, err := adapter.Start(ctx, harness.Session{
				Timeout: 8 * time.Minute,
				ID:      newSessionID(),
				Cwd:     checkout,
				// No permission_mode: the default is the classifier, which
				// is what this spec is about.
				Options: map[string]any{
					"model": "sonnet",
					"tools": map[string]any{"allow": []any{"Bash", "Read", "Glob", "Grep"}},
				},
				Events: log.sink(),
			}, harness.Prompt{Text: commitSkill() + "\n\n## The work\n\nThe change is already staged. Commit it.\n\nRefs: mill#24"}, nil)
			Expect(err).NotTo(HaveOccurred())
			log.watch(turn)
			terminal, err := turn.Wait(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)

			By("then the branch head is a new commit whose tree contains the change")
			after := headOf(checkout)
			Expect(after).NotTo(Equal(before), "no commit was made: %s", terminal.FinalMessage)
			Expect(git(checkout, "show", after+":README.md")).To(ContainSubstring("the change under test"))

			By("and no tool call in the step log was refused for approval")
			for _, event := range log.collected() {
				if result := event.GetToolResult(); result != nil {
					Expect(result.GetContent()).NotTo(ContainSubstring("requires approval"), "a tool call was refused for approval")
				}
			}
		})
	}
})

// scratchCheckout is a git repository with one staged change to README.md,
// as a run's workspace is when it reaches the commit step. Identity and
// signing are set on the repository itself so the spec does not depend on
// the account's global git configuration.
func scratchCheckout() string {
	GinkgoHelper()

	dir := GinkgoT().TempDir()
	git(dir, "init", "--initial-branch", "mill/spec")
	git(dir, "config", "user.email", "mill@localhost")
	git(dir, "config", "user.name", "mill")
	git(dir, "config", "commit.gpgsign", "false")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("# spec\n"), 0o600)).To(Succeed())
	git(dir, "add", "README.md")
	git(dir, "commit", "-m", "chore: seed the checkout")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("# spec\n\nthe change under test\n"), 0o600)).To(Succeed())
	git(dir, "add", "README.md")
	return dir
}

// commitSkill is the body of the skill the session is given, read from the
// workflow this repository seeds the store with: the spec runs the real
// instructions rather than a paraphrase of them.
func commitSkill() string {
	GinkgoHelper()

	body, err := os.ReadFile(filepath.Join("..", "..", "..", "workflows", "quick-fix", "skills", "commit", "SKILL.md"))
	Expect(err).NotTo(HaveOccurred())
	return string(body)
}

func headOf(dir string) string {
	GinkgoHelper()

	return strings.TrimSpace(git(dir, "rev-parse", "HEAD"))
}

// git runs one command in the checkout. The spec is the workspace here, so
// it spawns git itself rather than through one.
func git(dir string, args ...string) string {
	GinkgoHelper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testsupport.ScratchEnv()
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v: %s", args, out)
	return string(out)
}

// newSessionID mints the kind of id a run assigns a session: a UUID the
// harness accepts and has never seen.
func newSessionID() string {
	GinkgoHelper()

	raw := make([]byte, 16)
	_, err := rand.Read(raw)
	Expect(err).NotTo(HaveOccurred())
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// The reviewer, live: a real session running the real `review` skill against
// a two-criterion node, once on a change that satisfies it and once on a
// change that does not.
//
// This is the one thing no fake can answer. The mill's own specs prove what
// a verdict *does* — an approve closes the step, a revise sends the work
// back — by scripting the verdict; what they cannot prove is that a reviewer
// asked to judge a criterion nothing satisfies says no. The reference
// harness approved 59 nodes in a row without anyone knowing whether it
// could (docs/prd.md R-25).
var _ = Describe("The review skill against the real sidecar", Label("live"), func() {
	var entrypoint string

	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 to run the specs that spend a real session")
		}
		root, err := filepath.Abs(filepath.Join("..", "..", "..", "sidecar"))
		Expect(err).NotTo(HaveOccurred())
		build := exec.Command("pnpm", "run", "build")
		build.Dir = root
		build.Stdout, build.Stderr = GinkgoWriter, GinkgoWriter
		Expect(build.Run()).To(Succeed(), "the sidecar did not build")
		entrypoint = filepath.Join(root, "dist", "main.js")
	})

	// review runs one reviewing session over a checkout, under the step's
	// own settings: the model and effort `feature` gives it, the write tools
	// denied, and the verdict schema the mill will hold it to.
	review := func(checkout string) map[string]any {
		GinkgoHelper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		DeferCleanup(cancel)

		adapter := New(WithEntrypoint("node", entrypoint))
		turn, err := adapter.Start(ctx, harness.Session{
			Timeout: 8 * time.Minute,
			ID:      newSessionID(),
			Cwd:     checkout,
			Options: map[string]any{
				"model":  "opus",
				"effort": "high",
				"tools":  map[string]any{"deny": []any{"Write", "Edit", "Bash", "NotebookEdit"}},
			},
			Events: (&recorder{}).sink(),
		}, harness.Prompt{
			Text:         reviewSkill() + "\n\n## The node\n\n" + reviewNode + "\n\n" + changed(checkout),
			OutputSchema: verdictSchema(),
		}, nil)
		Expect(err).NotTo(HaveOccurred())

		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)
		AddReportEntry("verdict", terminal.StructuredOutput)
		Expect(terminal.StructuredOutput).NotTo(BeNil(), "the session emitted no verdict: %s", terminal.FinalMessage)
		return terminal.StructuredOutput
	}

	It("approves an implementation that satisfies every criterion", func() {
		verdict := review(reviewCheckout(true))

		Expect(verdict["decision"]).To(Equal("approve"))
	})

	It("revises an implementation with a criterion deliberately unmet, naming that criterion", func() {
		verdict := review(reviewCheckout(false))

		Expect(verdict["decision"]).To(Equal("revise"))
		blocking := []string{}
		findings, _ := verdict["findings"].([]any)
		for _, item := range findings {
			finding, ok := item.(map[string]any)
			if !ok || finding["severity"] != "blocking" {
				continue
			}
			blocking = append(blocking, fmt.Sprintf("%v %v", finding["criterion"], finding["summary"]))
		}
		Expect(blocking).NotTo(BeEmpty(), "a revise carries the finding that blocks it")
		Expect(strings.Join(blocking, "\n")).To(SatisfyAny(
			ContainSubstring("AC-2"), ContainSubstring("100"), ContainSubstring("too long")),
			"the blocking finding names the criterion that is unmet")
	})
})

// reviewNode is the work order the reviewer judges against: two acceptance
// criteria and one scenario, small enough that a session can hold all of it
// and specific enough that "unmet" is not a matter of taste.
const reviewNode = "# reverse: refuse an input that is too long\n\n" +
	"## Acceptance criteria\n\n" +
	"- [ ] AC-1: `Reverse` returns its input reversed.\n" +
	"- [ ] AC-2: `Reverse` returns an error, and the empty string, for an input longer than 100 bytes.\n\n" +
	"## Scenarios\n\n```gherkin\nScenario: An input that is too long is refused\n" +
	"  Given a string of 101 bytes\n  When it is reversed\n" +
	"  Then an error is returned\n  And the result is empty\n```\n"

// reviewCheckout is the change under review: the whole implementation and
// its test, either satisfying the node or with AC-2 and its scenario
// deliberately unmet — the code has no length check and the test that would
// have caught it does not exist.
func reviewCheckout(satisfied bool) string {
	GinkgoHelper()

	dir := GinkgoT().TempDir()
	body := "package reverse\n\nimport \"errors\"\n\n// Reverse returns s reversed.\nfunc Reverse(s string) (string, error) {\n"
	test := "package reverse\n\nimport \"testing\"\n\nfunc TestReverse(t *testing.T) {\n" +
		"\tif got, _ := Reverse(\"abc\"); got != \"cba\" {\n\t\tt.Fatalf(\"got %q\", got)\n\t}\n}\n"
	if satisfied {
		body += "\tif len(s) > 100 {\n\t\treturn \"\", errors.New(\"reverse: input is longer than 100 bytes\")\n\t}\n"
		test += "\n// An input that is too long is refused.\nfunc TestAnInputThatIsTooLongIsRefused(t *testing.T) {\n" +
			"\tgot, err := Reverse(string(make([]byte, 101)))\n\tif err == nil {\n\t\tt.Fatal(\"no error\")\n\t}\n" +
			"\tif got != \"\" {\n\t\tt.Fatalf(\"got %q\", got)\n\t}\n}\n"
	} else {
		body = strings.Replace(body, "import \"errors\"\n\n", "", 1)
	}
	body += "\tout := []rune(s)\n\tfor i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {\n" +
		"\t\tout[i], out[j] = out[j], out[i]\n\t}\n\treturn string(out), nil\n}\n"

	Expect(os.WriteFile(filepath.Join(dir, "reverse.go"), []byte(body), 0o600)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, "reverse_test.go"), []byte(test), 0o600)).To(Succeed())
	return dir
}

// changed stands in for the brief the mill puts in front of a reviewer
// (internal/run/verify.go): here the change itself, inline, because this
// spec's session has no checkout to run `git diff` in.
func changed(dir string) string {
	GinkgoHelper()

	var b strings.Builder
	b.WriteString("## The change you are reviewing\n\nThe whole of it, added by this run:\n")
	for _, name := range []string{"reverse.go", "reverse_test.go"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		Expect(err).NotTo(HaveOccurred())
		fmt.Fprintf(&b, "\n### %s\n\n```go\n%s```\n", name, content)
	}
	b.WriteString("\nThe test suite passes.\n")
	return b.String()
}

// reviewSkill is the body of the `review` skill this repository seeds the
// store with, and verdictSchema the schema its step names: the spec runs the
// real instructions against the real contract rather than a paraphrase.
func reviewSkill() string {
	GinkgoHelper()
	return readWorkflowFile("skills", "review", "SKILL.md")
}

func verdictSchema() string {
	GinkgoHelper()
	return readWorkflowFile("skills", "review", "verdict.schema.json")
}

func readWorkflowFile(parts ...string) string {
	GinkgoHelper()
	path := append([]string{"..", "..", "..", "workflows", "feature"}, parts...)
	body, err := os.ReadFile(filepath.Join(path...))
	Expect(err).NotTo(HaveOccurred())
	return string(body)
}

// The assessment, live: a real session running the real `build-workflow`
// skill over two nodes whose right answers are not in dispute — a typo, and
// a new subcommand with tests.
//
// Selection by assessment is a model deciding how much a change is worth
// (docs/domain-model.md §Selection), and no fake can answer whether it
// decides well. The mill's own specs prove what an assessment *does*: the
// named workflow is resolved in scope, the tier reaches the gate predicates,
// malformed output fails the step. What they cannot prove is that a skill
// asked to size a one-word fix says `quick-fix`.
var _ = Describe("The build-workflow skill against the real sidecar", Label("live"), func() {
	var entrypoint string

	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 to run the specs that spend a real session")
		}
		root, err := filepath.Abs(filepath.Join("..", "..", "..", "sidecar"))
		Expect(err).NotTo(HaveOccurred())
		build := exec.Command("pnpm", "run", "build")
		build.Dir = root
		build.Stdout, build.Stderr = GinkgoWriter, GinkgoWriter
		Expect(build.Run()).To(Succeed(), "the sidecar did not build")
		entrypoint = filepath.Join(root, "dist", "main.js")
	})

	// assess runs one assessing session over this repository, under the
	// step's own settings and the brief the mill renders for it: the skill,
	// the node, and the workflows in scope.
	assess := func(node string) map[string]any {
		GinkgoHelper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		DeferCleanup(cancel)

		repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
		Expect(err).NotTo(HaveOccurred())

		adapter := New(WithEntrypoint("node", entrypoint))
		turn, err := adapter.Start(ctx, harness.Session{
			Timeout: 8 * time.Minute,
			ID:      newSessionID(),
			Cwd:     repo,
			Options: map[string]any{
				"model":  "sonnet",
				"effort": "medium",
				"tools":  map[string]any{"allow": []any{"Glob", "Grep", "Read"}},
			},
			Events: (&recorder{}).sink(),
		}, harness.Prompt{
			Text:         assessSkill() + "\n\n## The node\n\n" + node + "\n\n" + workflowsInScope,
			OutputSchema: assessmentSchema(),
		}, nil)
		Expect(err).NotTo(HaveOccurred())

		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)
		AddReportEntry("assessment", terminal.StructuredOutput)
		Expect(terminal.StructuredOutput).NotTo(BeNil(), "the session emitted no assessment: %s", terminal.FinalMessage)
		return terminal.StructuredOutput
	}

	It("sizes a one-word fix as a quick fix", func() {
		assessment := assess(typoNode)

		Expect(assessment["tier"]).To(Equal("quick-fix"))
		Expect(assessment["workflow"]).To(Equal("quick-fix"))
		Expect(assessment["signals"]).NotTo(BeEmpty(), "the decision names the facts it rests on")
	})

	It("sizes a new subcommand with tests as a feature", func() {
		assessment := assess(subcommandNode)

		Expect(assessment["tier"]).To(Equal("feature"))
		Expect(assessment["workflow"]).To(Equal("feature"))
		Expect(assessment["signals"]).NotTo(BeEmpty(), "the decision names the facts it rests on")
	})
})

// typoNode and subcommandNode are the two work orders the live assessment is
// asked to size. They name this repository's own files, because the session
// reads the checkout it is standing in and a node about a repository it
// cannot see is not the question being asked.
const (
	typoNode = "# mill#000: Fix the typo in `mill run --skift-socket`'s usage string\n\n" +
		"## Context\n\n" +
		"The flag's usage string in `internal/cli/run.go` spells skift \"daggr\". It is one " +
		"word in one string literal.\n\n" +
		"## Acceptance criteria\n\n" +
		"- [ ] The usage string spells skift correctly\n" +
		"- [ ] `make lint` and `make test` pass\n"

	subcommandNode = "# mill#000: `mill runs` — list the runs in the ledger\n\n" +
		"## Context\n\n" +
		"An operator has no way to see which runs the ledger holds without reading the " +
		"database. Add a `mill runs` subcommand to the cobra tree that lists them — node, " +
		"workflow, status, current step — reading through the store service like every " +
		"other read command, with a `--status` filter.\n\n" +
		"## Deliverables\n\n" +
		"- `internal/cli/runs.go`: the command, and a `ListRuns` call added to the store " +
		"service in `proto/mill/v1/store.proto` and its handler\n" +
		"- `docs/tools.md`: what the command answers\n\n" +
		"## Acceptance criteria\n\n" +
		"- [ ] `mill runs` lists every run with its node, workflow, status and current step\n" +
		"- [ ] `--status waiting` lists only the runs waiting on a gate\n" +
		"- [ ] The listing is ordered newest first\n" +
		"- [ ] `make lint` and `make test` pass\n"

	// workflowsInScope is the brief the mill renders for an assessing
	// session, naming what it may choose between
	// (internal/run/select.go, candidateBrief).
	workflowsInScope = "## The workflows in scope\n\n" +
		"These are the workflows this run may be assessed into, read from the mill's " +
		"store. Name exactly one of them, spelled as it appears here; any other name " +
		"fails the selection step and the run walks nothing.\n\n" +
		"- `feature`\n- `quick-fix`\n"
)

// assessSkill is the body of the `build-workflow` skill this repository
// seeds the store with, and assessmentSchema the schema its step names.
func assessSkill() string {
	GinkgoHelper()
	return readAssessmentFile("SKILL.md")
}

func assessmentSchema() string {
	GinkgoHelper()
	return readAssessmentFile("assessment.schema.json")
}

func readAssessmentFile(name string) string {
	GinkgoHelper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..",
		"workflows", "build-workflow", "skills", "build-workflow", name))
	Expect(err).NotTo(HaveOccurred())
	return string(body)
}
