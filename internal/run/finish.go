package run

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/skift"
)

// recordUsage appends what an attempt billed to the ledger, one row per
// model. The adapter is the only component that sees the harness's numbers,
// so this is where they enter the record.
func (r *Runner) recordUsage(ctx context.Context, s *state, sr *millv1.StepRun, usage harness.Usage) error {
	if len(usage) == 0 {
		return nil
	}
	rows := make([]*millv1.Usage, 0, len(usage))
	for _, model := range usage.Models() {
		counts := usage[model]
		rows = append(rows, &millv1.Usage{
			StepRunId:        sr.GetId(),
			Model:            model,
			InputTokens:      counts.Input,
			OutputTokens:     counts.Output,
			CacheReadTokens:  counts.CacheRead,
			CacheWriteTokens: counts.CacheWrite,
			RecordedAt:       timestamppb.New(r.options.Now()),
			Source:           millv1.UsageSource_USAGE_SOURCE_SDK,
		})
	}
	_, err := call(ctx, r.options.Store.RecordUsage, &millv1.RecordUsageRequest{
		LeaseId:   s.leaseID(),
		StepRunId: sr.GetId(),
		Usage:     rows,
	})
	return err
}

// recordGatewayUsage appends what the credential gateway counted on one
// response, as its own row with source `gateway`: the same shape as the
// adapter's report, distinct from it, so the two can be read beside each
// other (docs/sandbox.md §Credentials). It is never posted to skift; the
// adapter's rows are the step's cost there.
func (r *Runner) recordGatewayUsage(ctx context.Context, lease string, sr *millv1.StepRun, usage gateway.Usage) error {
	_, err := call(ctx, r.options.Store.RecordUsage, &millv1.RecordUsageRequest{
		LeaseId:   lease,
		StepRunId: sr.GetId(),
		Usage: []*millv1.Usage{{
			StepRunId:        sr.GetId(),
			Model:            usage.Model,
			InputTokens:      usage.Input,
			OutputTokens:     usage.Output,
			CacheReadTokens:  usage.CacheRead,
			CacheWriteTokens: usage.CacheWrite,
			RecordedAt:       timestamppb.New(r.options.Now()),
			Source:           millv1.UsageSource_USAGE_SOURCE_GATEWAY,
		}},
	})
	return err
}

// postUsage sends an attempt's totals to skift, one record per model, and
// marks the rows posted. The totals are the adapter's rows alone: the
// store keeps the gateway's count out of them (mill.v1.StepRunDetail).
//
// It runs whether the attempt closed or failed, and always before anything
// that releases the lease — a release ends the lease ReportUsage needs, and
// the completing patch releases it as part of itself, so the last step's
// usage is posted before that patch and not after it (docs/ledger.md
// §Resume).
func (r *Runner) postUsage(ctx context.Context, s *state, sr *millv1.StepRun) error {
	detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
	if err != nil {
		return err
	}
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetId() != sr.GetId() {
			continue
		}
		for _, totals := range d.GetTotals() {
			if err := s.skift.ReportUsage(ctx, s.leaseID(), s.node.GetReference(), totals.GetModel(), skift.TokenCounts{
				InputTokens:      totals.GetInputTokens(),
				OutputTokens:     totals.GetOutputTokens(),
				CacheReadTokens:  totals.GetCacheReadTokens(),
				CacheWriteTokens: totals.GetCacheWriteTokens(),
			}); err != nil {
				return err
			}
		}
	}
	if _, err := call(ctx, r.options.Store.MarkUsagePosted, &millv1.MarkUsagePostedRequest{
		LeaseId:   s.leaseID(),
		StepRunId: sr.GetId(),
	}); err != nil {
		return err
	}
	return nil
}

// finish is the tail of the run, in the one order that is crash-safe:
//
//  1. the completing patch's request id, minted and stored before use;
//  2. the patch itself, which carries the result and its artifacts and
//     releases the lease as part of the same write;
//  3. store.Done, only once skift has acknowledged the patch;
//  4. the workspace, destroyed, and its end recorded: the files gone and
//     what its caches promoted (docs/ledger.md §workspaces).
//
// Never the reverse. A run marked done before skift confirmed would strand
// a node nothing can ever claim; a crash between 2 and 3 is recovered by the
// next `mill run`, which finds the node done and marks the run done under
// the request id already stored (docs/architecture.md §"Write ordering").
func (r *Runner) finish(ctx context.Context, s *state, detail *millv1.InspectResponse) error {
	keyed, err := call(ctx, r.options.Store.SetCompleteRequestId, &millv1.SetCompleteRequestIdRequest{
		LeaseId:   s.leaseID(),
		RunId:     s.run.GetId(),
		RequestId: skift.RequestID(s.run.GetId(), completeStep, s.run.GetGeneration()),
	})
	if err != nil {
		return err
	}
	requestID := keyed.GetRun().GetCompleteRequestId()

	if err := s.skift.Complete(ctx, requestID, s.leaseID(), s.node.GetReference(), r.result(s, detail)); err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.Done, &millv1.DoneRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
	}); err != nil {
		return err
	}
	if err := s.workspace.Destroy(ctx); err != nil {
		// The files are still there, so the record stays live: the
		// reconciliation pass finds the machine by the run's name.
		r.options.Log.Warn("destroying the workspace", "run", s.run.GetId(), "error", err)
	} else if err := r.finishWorkspace(ctx, s); err != nil {
		// The run is done and the node complete; the record of the
		// workspace's end is a log line short, never a failed run.
		r.options.Log.Warn("recording the workspace's end", "run", s.run.GetId(), "error", err)
	}
	r.printf("%s done\n", s.node.GetReference())
	return nil
}

// completeStep is the step half of the completing patch's request id. The
// patch belongs to the run rather than to a step, and the generation stands
// in for the attempt, so a restart's completing patch is a different write.
const completeStep = "complete"

// result is what the node is completed with: the implementer's summary as
// the body, and the run's references and usage as artifacts. Every
// artifact's metadata carries the run id (docs/ledger.md §"What goes to
// skift, and when").
func (r *Runner) result(s *state, detail *millv1.InspectResponse) *skiftv1.Result {
	result := &skiftv1.Result{Body: s.summary}
	if result.Body == "" {
		result.Body = fmt.Sprintf("Run %s of %s completed every step of workflow %s.",
			s.run.GetId(), s.node.GetReference(), s.workflow.GetName())
	}
	if s.mergeCommit != "" {
		result.Artifacts = append(result.Artifacts, reference(s, "merge-commit",
			"The commit the run's branch became on the default branch.", s.mergeCommit))
	}
	if s.pullRequestURL != "" {
		result.Artifacts = append(result.Artifacts, reference(s, "pull-request",
			"The pull request the run opened.", s.pullRequestURL))
	}
	result.Artifacts = append(result.Artifacts, text(s, "usage",
		"What the run cost, by model.", usageSummary(detail)))
	return result
}

// reference is an artifact naming something outside skift: a commit, a
// pull request. docs/ledger.md calls both `reference`.
//
// Skift requires a sha256 digest on a reference part. Mill does not fetch
// what the reference points at — the commit is on a forge, and pulling it
// back to hash it would prove nothing the forge has not already said — so
// the digest is over the reference itself. It identifies what was recorded,
// which is the fact mill is attesting to; byte integrity of the referenced
// content is the forge's claim and not mill's.
func reference(s *state, name, description, value string) *skiftv1.Artifact {
	return &skiftv1.Artifact{
		Name:        name,
		Description: description,
		Parts: []*skiftv1.Part{{
			Content: &skiftv1.Part_Reference{Reference: &skiftv1.ReferencePart{
				Uri:       value,
				Digest:    digest(value),
				MediaType: "text/plain",
				Size:      int64(len(value)),
			}},
		}},
		Metadata: metadata(s, name),
	}
}

// text is an artifact carrying its own content: the usage summary, and the
// plan and verdicts a longer workflow produces.
func text(s *state, name, description, body string) *skiftv1.Artifact {
	return &skiftv1.Artifact{
		Name:        name,
		Description: description,
		Parts: []*skiftv1.Part{{
			Content: &skiftv1.Part_Text{Text: &skiftv1.TextPart{
				Text:      body,
				MediaType: "text/markdown",
			}},
		}},
		Metadata: metadata(s, name),
	}
}

// metadata is what every artifact mill attaches carries: the run it belongs
// to, so a reader can join skift's side of a run to the ledger's.
func metadata(s *state, name string) *structpb.Struct {
	value, err := structpb.NewStruct(map[string]any{
		"run_id":   s.run.GetId(),
		"node_ref": s.node.GetReference(),
		"artifact": name,
	})
	if err != nil {
		// Every value above is a string, which structpb accepts.
		return nil
	}
	return value
}

// usageSummary is the run's cost, by model, as a table.
func usageSummary(detail *millv1.InspectResponse) string {
	totals := map[string]*millv1.ModelTotals{}
	for _, d := range detail.GetSteps() {
		for _, t := range d.GetTotals() {
			sum, ok := totals[t.GetModel()]
			if !ok {
				totals[t.GetModel()] = &millv1.ModelTotals{
					Model:            t.GetModel(),
					InputTokens:      t.GetInputTokens(),
					OutputTokens:     t.GetOutputTokens(),
					CacheReadTokens:  t.GetCacheReadTokens(),
					CacheWriteTokens: t.GetCacheWriteTokens(),
				}
				continue
			}
			sum.InputTokens += t.GetInputTokens()
			sum.OutputTokens += t.GetOutputTokens()
			sum.CacheReadTokens += t.GetCacheReadTokens()
			sum.CacheWriteTokens += t.GetCacheWriteTokens()
		}
	}
	models := make([]string, 0, len(totals))
	for model := range totals {
		models = append(models, model)
	}
	sort.Strings(models)

	var b strings.Builder
	b.WriteString("| model | input | output | cache read | cache write |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, model := range models {
		t := totals[model]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d |\n",
			model, t.GetInputTokens(), t.GetOutputTokens(), t.GetCacheReadTokens(), t.GetCacheWriteTokens())
	}
	return b.String()
}

// neverRan is the exit of a failure with no process behind it.
func neverRan() harnessv1.ProcessExit {
	return harnessv1.ProcessExit{Status: -1, Reason: harnessv1.ExitReason_EXIT_REASON_REFUSED}
}
