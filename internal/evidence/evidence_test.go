package evidence_test

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/evidence"
	"github.com/steading-ai/mill/internal/forge"
)

// branch is the run's working branch, as workspace.BranchFor derives it from
// the node this work was filed as.
const branch = "mill/mill-14"

var _ = Describe("The commit verifier (verifyCommit)", Label("unit"), func() {
	var (
		ctx  context.Context
		repo *repository
		base,
		onBranch,
		offBranch string
	)

	// mill is the identity the workspace commits under, which is what the
	// verifier compares a commit's author and committer against.
	mill := evidence.Committer{Name: "mill", Email: "mill@example.test"}

	BeforeEach(func() {
		ctx = context.Background()
		repo = newRepository(branch)
		// The head the commit step started at: what the branch already
		// pointed at, and what closing on it would be closing on nothing.
		base = repo.commit("README.md", "the default branch")
		onBranch = repo.commit("change.txt", "the run's own work")
		// A commit that exists in the repository and is not on the run's
		// branch: made on another branch, which is where a session that
		// wandered off the run's claim would leave one.
		repo.must("checkout", "--quiet", "-b", "elsewhere")
		offBranch = repo.commit("stray.txt", "somewhere else")
		repo.must("checkout", "--quiet", branch)
	})

	It("The agent saying it committed is not evidence", func() {
		// The session ends by saying it committed. Nothing observed a
		// commit, and no such commit is on the branch.
		step := evidence.Step{
			Name:       "commit",
			Definition: &millv1.Step{Name: "commit", Kind: millv1.StepKind_STEP_KIND_SKILL},
			Branch:     branch,
			Base:       base,
			Committer:  mill,
			Git:        repo,
			Session:    session("Done — committed as abc123.", nil),
		}

		result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		// Verification fails, and the step stays open: there is no evidence
		// row to close it with.
		var refused *evidence.NotVerifiedError
		Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
		Expect(refused.Reason).To(ContainSubstring("no commit was recorded"))
		Expect(result.Value).To(BeNil())
		Expect(evidence.Pending(err)).To(BeFalse())
		// And what the session said was never taken for a fact to check.
		Expect(repo.asked("abc123")).To(BeFalse())
	})

	It("refuses a SHA that is not on the run's branch even though the object exists", func() {
		step := evidence.Step{Name: "commit", Branch: branch, Base: base, Committer: mill,
			Git: repo, Claim: offBranch}

		result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("is not on " + branch)))
		// The object does exist: git was asked, and answered.
		Expect(repo.asked("cat-file")).To(BeTrue())
		out, catErr := repo.Run(ctx, nil, "cat-file", "-e", offBranch+"^{commit}")
		Expect(catErr).NotTo(HaveOccurred(), out)
	})

	It("closes the step with a commit that resolves on the run's branch", func() {
		step := evidence.Step{Name: "commit", Branch: branch, Base: base, Committer: mill,
			Git: repo, Claim: onBranch,
			Now: func() time.Time { return time.Unix(1700000000, 0) }}

		result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_COMMIT))
		Expect(result.Value.GetCommit()).To(Equal(onBranch))
		Expect(result.Value.GetVerifiedBy()).To(Equal("evidence.commit"))
		Expect(result.Value.GetVerifiedAt().AsTime()).To(BeTemporally("==", time.Unix(1700000000, 0).UTC()))
	})

	It("refuses the head the step started at", func() {
		// The branch head is the base: whatever the session did, it did not
		// commit, and the commit already on the branch is not this step's.
		step := evidence.Step{Name: "commit", Branch: branch, Base: base, Committer: mill,
			Git: repo, Claim: base}

		result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring(base + " is the head the step started at")))
	})

	It("refuses a commit that does not descend from the base", func() {
		// The base is on another branch entirely, so the claim is on the
		// run's branch and yet says nothing about what this step added.
		step := evidence.Step{Name: "commit", Branch: branch, Base: offBranch, Committer: mill,
			Git: repo, Claim: onBranch}

		result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("does not descend from " + offBranch)))
	})

	It("refuses a commit made under another identity, naming the identity found", func() {
		theirs := repo.commitAs("Someone Else", "else@example.test", "theirs.txt", "not the mill's work")
		step := evidence.Step{Name: "commit", Branch: branch, Base: base, Committer: mill,
			Git: repo, Claim: theirs}

		result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("Someone Else <else@example.test>")))
		Expect(err).To(MatchError(ContainSubstring("not by mill <mill@example.test>")))
	})

	It("refuses a claim with no base commit recorded for the attempt", func() {
		step := evidence.Step{Name: "commit", Branch: branch, Committer: mill,
			Git: repo, Claim: onBranch}

		_, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(err).To(MatchError(ContainSubstring("no base commit was recorded")))
	})

	It("refuses a claim that is not a git object name without asking git", func() {
		repo.calls = nil
		step := evidence.Step{Name: "commit", Branch: branch, Base: base, Committer: mill,
			Git: repo, Claim: "--upload-pack=touch /tmp/x"}

		_, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

		Expect(err).To(MatchError(ContainSubstring("is not a git object name")))
		Expect(repo.calls).To(BeEmpty())
	})

	Describe("an unmoved head after a revise", func() {
		// onBranch stands for the commit an earlier attempt at this step
		// made; the branch has not moved since. The step's earliest base —
		// where it started before any revise — is `base`, and this
		// attempt's own base (the head it was handed) is `onBranch` itself.
		It("closes on the head when the tree is clean and it descends from the step's earliest base", func() {
			step := evidence.Step{Name: "commit", Branch: branch, Base: onBranch, EarliestBase: base,
				Committer: mill, Git: repo, Claim: onBranch}

			result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Value.GetCommit()).To(Equal(onBranch))
		})

		It("refuses a dirty tree, naming the session's failure to commit", func() {
			repo.write("scratch.txt", "an edit nobody committed")
			step := evidence.Step{Name: "commit", Branch: branch, Base: onBranch, EarliestBase: base,
				Committer: mill, Git: repo, Claim: onBranch}

			result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

			Expect(result.Value).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring(
				"the branch head is still " + onBranch + ": the session made no commit")))
		})

		It("refuses a head that is the earliest base itself", func() {
			step := evidence.Step{Name: "commit", Branch: branch, Base: base, EarliestBase: base,
				Committer: mill, Git: repo, Claim: base}

			result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

			Expect(result.Value).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring(base + " is the head the step started at")))
		})

		It("refuses a commit made under another identity, even though the head has not moved", func() {
			theirs := repo.commitAs("Someone Else", "else@example.test", "theirs.txt", "not the mill's work")
			step := evidence.Step{Name: "commit", Branch: branch, Base: theirs, EarliestBase: base,
				Committer: mill, Git: repo, Claim: theirs}

			result, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, step)

			Expect(result.Value).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("Someone Else <else@example.test>")))
		})
	})
})

var _ = Describe("The exit verifier", Label("unit"), func() {
	check := func(codes ...int32) evidence.Step {
		return evidence.Step{
			Name: "clean-tree",
			Definition: &millv1.Step{
				Name:  "clean-tree",
				Kind:  millv1.StepKind_STEP_KIND_CHECK,
				Check: &millv1.Check{Command: []string{"scripts/clean-tree"}, Exit: codes},
			},
		}
	}

	It("closes the step when the process exited with a code the definition names", func() {
		step := check()
		step.Exit = &harnessv1.ProcessExit{Status: 0}

		result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_EXIT, step)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetExit().GetStatus()).To(BeZero())
		// The check is what verified it, by name.
		Expect(result.Value.GetVerifiedBy()).To(Equal("check clean-tree"))
	})

	It("refuses a status the definition does not name", func() {
		step := check(0)
		step.Exit = &harnessv1.ProcessExit{Status: 1}

		result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_EXIT, step)

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("exited 1, not 0")))
	})

	It("refuses a process that never ran, whatever the definition would accept", func() {
		step := check(0, -1)
		step.Exit = &harnessv1.ProcessExit{Status: -1, Reason: harnessv1.ExitReason_EXIT_REASON_TIMEOUT}

		_, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_EXIT, step)

		Expect(err).To(MatchError(ContainSubstring("never ran (timeout)")))
	})

	// A preparatory step's evidence is the record of how its command ended,
	// whichever way: a non-zero exit and a spent budget close it as zero
	// does, because its failure is a finding and never a failed attempt
	// (docs/sandbox.md §"Preparatory steps").
	DescribeTable("closes a preparatory step on the record of its exit, whichever way it ended",
		func(exit *harnessv1.ProcessExit) {
			step := evidence.Step{
				Name: "toolchain",
				Definition: &millv1.Step{
					Name:  "toolchain",
					Kind:  millv1.StepKind_STEP_KIND_PREPARATORY,
					Check: &millv1.Check{Command: []string{"sh", "-c", "mise trust /work && mise install"}},
				},
				Exit: exit,
			}

			result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_EXIT, step)

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Value.GetExit().GetStatus()).To(Equal(exit.GetStatus()))
			Expect(result.Value.GetExit().GetReason()).To(Equal(exit.GetReason()))
			Expect(result.Value.GetVerifiedBy()).To(Equal("preparatory toolchain"))
		},
		Entry("exited 0", &harnessv1.ProcessExit{Status: 0}),
		Entry("exited 1", &harnessv1.ProcessExit{Status: 1}),
		Entry("stopped at its budget", &harnessv1.ProcessExit{Status: -1, Reason: harnessv1.ExitReason_EXIT_REASON_TIMEOUT}),
	)

	It("still refuses a preparatory step with no process behind it", func() {
		step := evidence.Step{
			Name:       "toolchain",
			Definition: &millv1.Step{Name: "toolchain", Kind: millv1.StepKind_STEP_KIND_PREPARATORY},
		}

		_, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_EXIT, step)

		Expect(err).To(MatchError(ContainSubstring("no process ran")))
	})
})

var _ = Describe("The verdict verifier", Label("unit"), func() {
	step := func(output map[string]any) evidence.Step {
		return evidence.Step{
			Name:       "review",
			Definition: &millv1.Step{Name: "review", Kind: millv1.StepKind_STEP_KIND_SKILL},
			Session:    session("I have reviewed the change.", output),
		}
	}
	verify := func(output map[string]any) (evidence.Evidence, error) {
		return evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_VERDICT, step(output))
	}

	It("closes the step on a verdict that approves", func() {
		result, err := verify(map[string]any{"decision": "approve"})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetVerdict().GetDecision()).To(Equal(millv1.VerdictDecision_VERDICT_DECISION_APPROVE))
		Expect(result.Warnings).To(BeEmpty())
	})

	It("refuses a revise with no blocking finding as malformed, naming the field", func() {
		result, err := verify(map[string]any{
			"decision": "revise",
			"findings": []any{map[string]any{
				"criterion": "AC-2",
				"severity":  "note",
				"summary":   "the comment above Verify could say more",
			}},
		})

		// A reviewer that asks for changes and names none has asked for
		// nothing: it is neither an approve nor a revise, so it is the
		// reviewer's own failure and the step is failed as malformed.
		Expect(err).To(MatchError(ContainSubstring(`severity "blocking"`)))
		Expect(evidence.Malformed(err)).To(BeTrue())
		Expect(result.Value).To(BeNil())
		Expect(result.Verdict).To(BeNil())
	})

	It("refuses a revise that blocks, and keeps the findings for the revise prompt", func() {
		result, err := verify(map[string]any{
			"decision": "revise",
			"findings": []any{map[string]any{
				"criterion": "AC-1",
				"severity":  "blocking",
				"summary":   "the status verifier treats pending as failure",
				"location":  "internal/evidence/evidence.go:1",
			}},
		})

		Expect(err).To(MatchError(ContainSubstring("1 blocking finding")))
		Expect(result.Value.GetVerdict().GetDecision()).To(Equal(millv1.VerdictDecision_VERDICT_DECISION_REVISE),
			"a revise is still evidence: it is recorded on the failed attempt, not used to close it")
		Expect(result.Verdict.GetFindings()).To(HaveLen(1))
		Expect(result.Verdict.GetFindings()[0].GetSummary()).To(Equal("the status verifier treats pending as failure"))
	})

	It("records a finding that names no criterion as a note", func() {
		result, err := verify(map[string]any{
			"decision": "approve",
			"findings": []any{map[string]any{"severity": "blocking", "summary": "this feels wrong"}},
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetVerdict().GetFindings()[0].GetSeverity()).
			To(Equal(millv1.FindingSeverity_FINDING_SEVERITY_NOTE))
		Expect(result.Warnings).To(ContainElement(ContainSubstring("names no acceptance criterion")))
	})

	It("refuses a revise whose only blocking finding names no criterion", func() {
		// The finding is demoted to a note, so nothing blocks: a reviewer
		// drifting off the contract cannot send the work back on it.
		_, err := verify(map[string]any{
			"decision": "revise",
			"findings": []any{map[string]any{"severity": "blocking", "summary": "this feels wrong"}},
		})

		Expect(evidence.Malformed(err)).To(BeTrue())
	})

	It("refuses output that is not a verdict", func() {
		_, err := verify(map[string]any{"decision": "needs-human"})

		Expect(err).To(MatchError(ContainSubstring("/decision is not one of \"approve\", \"revise\"")))
	})

	It("refuses a session that produced no structured output", func() {
		_, err := verify(nil)

		Expect(err).To(MatchError(ContainSubstring("no structured output")))
	})
})

var _ = Describe("The structured verifier", Label("unit"), func() {
	const schema = `{
      "type": "object",
      "required": ["tier", "findings"],
      "properties": {
        "tier": {"type": "string", "enum": ["quick-fix", "small", "feature", "large"]},
        "findings": {
          "type": "array",
          "items": {"type": "object", "required": ["summary"],
                    "properties": {"summary": {"type": "string"}, "weight": {"type": "integer"}}}
        }
      }
    }`

	step := func(output map[string]any) evidence.Step {
		return evidence.Step{
			Name:       "assess",
			Definition: &millv1.Step{Name: "assess", Kind: millv1.StepKind_STEP_KIND_SKILL, OutputSchema: schema},
			Session:    session("assessed", output),
		}
	}
	verify := func(output map[string]any) (evidence.Evidence, error) {
		return evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED, step(output))
	}

	It("names the first violating path when the output does not validate", func() {
		result, err := verify(map[string]any{
			"tier": "feature",
			"findings": []any{
				map[string]any{"summary": "fine", "weight": 2},
				map[string]any{"summary": "also fine", "weight": "heavy"},
			},
		})

		Expect(result.Value).To(BeNil())
		var invalid *evidence.SchemaError
		Expect(errors.As(err, &invalid)).To(BeTrue(), "%v", err)
		Expect(invalid.Path).To(Equal("/findings/1/weight"))
		Expect(err).To(MatchError(ContainSubstring("/findings/1/weight is a string, not integer")))
	})

	It("names the missing property when a required one is absent", func() {
		_, err := verify(map[string]any{"tier": "small"})

		Expect(err).To(MatchError(ContainSubstring("/findings is required and missing")))
	})

	It("closes the step with the validated document", func() {
		result, err := verify(map[string]any{"tier": "small", "findings": []any{}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetStructured().GetFields()["tier"].GetStringValue()).To(Equal("small"))
	})

	It("refuses a step that declares no schema, because nothing would validate the output", func() {
		step := step(map[string]any{"tier": "small"})
		step.Definition.OutputSchema = ""

		_, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED, step)

		Expect(err).To(MatchError(ContainSubstring("declares no output schema")))
	})
})

var _ = Describe("The url verifier", Label("unit"), func() {
	It("closes the step with the URL the forge returns for the run's branch", func() {
		remote := &fixtureForge{pr: forge.PullRequest{Number: 7, Branch: branch,
			URL: "https://gitlab.example.test/group/thing/-/merge_requests/7", State: "open"}}

		result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_URL,
			evidence.Step{Name: "pr", Branch: branch, Forge: remote,
				Session: session("Opened https://gitlab.example.test/group/thing/-/merge_requests/9", nil)})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetUrl()).To(Equal("https://gitlab.example.test/group/thing/-/merge_requests/7"))
		Expect(remote.asked).To(ConsistOf("pr " + branch))
	})

	It("refuses a branch the forge has no pull request for", func() {
		remote := &fixtureForge{prErr: forge.ErrNoPullRequest}

		result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_URL,
			evidence.Step{Name: "pr", Branch: branch, Forge: remote})

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("no pull request for " + branch)))
		Expect(errors.Is(err, forge.ErrNoPullRequest)).To(BeTrue())
	})
})

var _ = Describe("The status verifier", Label("unit"), func() {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	step := func(status forge.Status) evidence.Step {
		return evidence.Step{
			Name:   "ci",
			Claim:  sha,
			Forge:  &fixtureForge{status: status},
			Branch: branch,
			Workflow: &millv1.Workflow{Status: &millv1.StatusPolicy{
				Interval: durationpb.New(30 * time.Second),
				Timeout:  durationpb.New(10 * time.Minute),
			}},
		}
	}
	verify := func(s evidence.Step) (evidence.Evidence, error) {
		return evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_STATUS, s)
	}

	It("takes success as evidence", func() {
		result, err := verify(step(forge.Success))

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetStatus()).To(Equal("success"))
	})

	It("takes pending as not yet, leaving the step open to be retried on the watch interval", func() {
		result, err := verify(step(forge.Pending))

		Expect(result.Value).To(BeNil(), "a pending pipeline closes nothing")
		Expect(evidence.Pending(err)).To(BeTrue(), "%v", err)
		var pending *evidence.PendingError
		Expect(errors.As(err, &pending)).To(BeTrue())
		Expect(pending.RetryAfter).To(Equal(30 * time.Second))
	})

	It("takes failed as failure", func() {
		result, err := verify(step(forge.Failed))

		Expect(result.Value).To(BeNil())
		Expect(evidence.Pending(err)).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("as failed")))
	})

	It("watches on the documented interval and cap when the workflow names none", func() {
		s := step(forge.Pending)
		s.Workflow = nil

		_, err := verify(s)

		var pending *evidence.PendingError
		Expect(errors.As(err, &pending)).To(BeTrue(), "%v", err)
		Expect(pending.RetryAfter).To(Equal(evidence.DefaultStatusInterval))
		Expect(evidence.DefaultStatusTimeout).To(Equal(time.Hour))
	})

	It("gives up once the checks have not settled within the workflow's cap", func() {
		now := time.Now()
		s := step(forge.Pending)
		s.Now = func() time.Time { return now }
		// Two reads of the same head: the fold has to pick the earliest one,
		// not just any entry, to measure the head's whole wait.
		s.StatusHistory = []evidence.StatusRead{
			{Head: sha, At: now.Add(-3 * time.Minute)},
			{Head: sha, At: now.Add(-11 * time.Minute)},
		}

		_, err := verify(s)

		Expect(evidence.Pending(err)).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("had not settled after 10m0s")))
	})

	It("gives a freshly read head its own window, however long an earlier head waited", func() {
		now := time.Now()
		s := step(forge.Pending)
		s.Now = func() time.Time { return now }
		// A different head, long past the cap: its wait must not count
		// against sha, which this history has never mentioned (mill#91).
		s.StatusHistory = []evidence.StatusRead{
			{Head: strings.Repeat("f", 40), At: now.Add(-61 * time.Minute)},
		}

		_, err := verify(s)

		Expect(evidence.Pending(err)).To(BeTrue(), "%v", err)
	})
})

var _ = Describe("The artifact verifier", Label("unit"), func() {
	const digest = "sha256:9f2c"

	It("closes the step with a digest skift accepted", func() {
		result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
			evidence.Step{Name: "gather-context", Claim: digest, Artifacts: fixtureArtifacts{held: digest}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetArtifact()).To(Equal(digest))
	})

	It("refuses a digest skift does not hold", func() {
		result, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
			evidence.Step{Name: "gather-context", Claim: digest, Artifacts: fixtureArtifacts{held: "sha256:0000"},
				Session: session("I attached the context as "+digest, nil)})

		Expect(result.Value).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("holds no artifact with digest")))
	})
})

var _ = Describe("The gate verifier", Label("unit"), func() {
	const node = "01JB3Z"

	verify := func(gates evidence.Gates) (evidence.Evidence, error) {
		return evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_GATE,
			evidence.Step{Name: "merge", Claim: node, Gates: gates})
	}

	It("closes the step when the gate node is done with a result attached", func() {
		result, err := verify(fixtureGates{node: evidence.GateNode{Status: "done", Result: "Merged."}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Value.GetGate()).To(Equal(node))
	})

	It("refuses a gate node that is done with no result attached", func() {
		_, err := verify(fixtureGates{node: evidence.GateNode{Status: "done"}})

		Expect(err).To(MatchError(ContainSubstring("no result attached")))
	})

	It("refuses a dropped gate, which is the human's no", func() {
		result, err := verify(fixtureGates{node: evidence.GateNode{Status: "dropped"}})

		Expect(result.Value).To(BeNil())
		Expect(evidence.Pending(err)).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("the decision is no")))
	})

	It("takes an open gate as not yet", func() {
		_, err := verify(fixtureGates{node: evidence.GateNode{Status: "open"}})

		Expect(evidence.Pending(err)).To(BeTrue(), "%v", err)
	})
})

var _ = Describe("What a verifier may consult", Label("unit"), func() {
	// Every kind, wired so that the world says no and the session says yes:
	// the seams refuse, and the session's final message and transcript are
	// full of the facts that would close the step if anything read them.
	var (
		log  *transcript
		repo *repository
		all  []millv1.EvidenceKind
	)

	const claimed = "0123456789abcdef0123456789abcdef01234567"

	BeforeEach(func() {
		repo = newRepository(branch)
		repo.commit("README.md", "the default branch")
		log = newTranscript(`{"role":"assistant","text":"committed ` + claimed + `, CI is green"}`)
		all = []millv1.EvidenceKind{
			millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
			millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
			millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
			millv1.EvidenceKind_EVIDENCE_KIND_URL,
			millv1.EvidenceKind_EVIDENCE_KIND_STATUS,
			millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
			millv1.EvidenceKind_EVIDENCE_KIND_GATE,
		}
	})

	step := func() evidence.Step {
		s := evidence.Step{
			Name: "step",
			Definition: &millv1.Step{Name: "step", Kind: millv1.StepKind_STEP_KIND_CHECK,
				Check: &millv1.Check{Command: []string{"scripts/clean-tree"}}},
			Branch:    branch,
			Git:       repo,
			Exit:      &harnessv1.ProcessExit{Status: 1},
			Forge:     &fixtureForge{prErr: forge.ErrNoPullRequest, status: forge.Failed},
			Artifacts: fixtureArtifacts{held: "sha256:0000"},
			Gates:     fixtureGates{node: evidence.GateNode{Status: "dropped"}},
			Claim:     claimed,
		}
		s.Session = session("I committed "+claimed+", CI is green, the PR is https://example.test/1, approved.", nil)
		s.Session.Transcript = log.evidence()
		return s
	}

	It("No verifier opens the session's transcript", func() {
		for _, kind := range all {
			_, err := evidence.Verify(context.Background(), kind, step())
			Expect(err).To(HaveOccurred(), "%s should not have been verified", kind)
		}

		Expect(log.opens).To(BeZero(), "a transcript is not evidence and nothing may read one")
		Expect(repo.asked(log.path)).To(BeFalse())
	})

	It("consults no session's final message for any kind but verdict and structured", func() {
		for _, kind := range all {
			result, err := evidence.Verify(context.Background(), kind, step())

			// Every seam said no. The final message says yes for all of
			// them, and every kind is refused anyway.
			Expect(result.Value).To(BeNil(), "%s", kind)
			Expect(err).To(HaveOccurred(), "%s", kind)
			Expect(err.Error()).NotTo(ContainSubstring("CI is green"))
		}
		// The two kinds that read a session read its structured output, and
		// there was none.
		for _, kind := range []millv1.EvidenceKind{
			millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
			millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
		} {
			_, err := evidence.Verify(context.Background(), kind, step())
			Expect(err).To(MatchError(Or(
				ContainSubstring("no structured output"),
				ContainSubstring("declares no output schema"))), "%s", kind)
		}
	})
})

var _ = Describe("Verify", Label("unit"), func() {
	It("refuses a kind no verifier covers", func() {
		_, err := evidence.Verify(context.Background(), millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED, evidence.Step{})

		Expect(err).To(MatchError(ContainSubstring("no verifier")))
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("unspecified"))
	})
})
