// Package run is the loop: claim a node, hold its lease, walk the workflow's
// steps, and finish.
//
// docs/ledger.md §Resume is the specification, line by line, and this package
// is that pseudocode in Go. Three of its properties are the reason it is
// shaped the way it is:
//
//   - Steps are re-run, sessions are resumed. An interrupted step starts a
//     new attempt from the top; the session it runs in is found by its
//     deterministic id, so the model picks up its own context.
//   - Resume is the same code as a first run. The only branch a resume takes
//     that a first run does not is the gate at the top of the loop.
//   - Dagger first, store second. The node is completed in dagger before the
//     run is marked done in the store, under a request id minted and stored
//     before the patch is sent, so a crash between the two is a retry of the
//     same patch and never a node nothing can claim.
//
// A fenced write on either side — the store refusing a lease that is no
// longer the run's, dagger refusing the same — is the normal end of a stale
// worker, not an error to retry. The run is abandoned locally, nothing more
// is written, and [ErrFenced] comes back.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/notify"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
)

// Defaults from docs/architecture.md §Heartbeat: dagger's lease TTL is
// fifteen minutes, and the worker renews well inside it.
const (
	// DefaultTTL is the lease TTL every claim asks for.
	DefaultTTL = 15 * time.Minute
	// DefaultGrace is how long a cancelled step has to stop before the
	// adapter kills it.
	DefaultGrace = 5 * time.Second
)

// ErrFenced ends a run that lost its lease. It is returned rather than
// wrapped in a failure, because there is nothing to recover: the ledger is
// the truth and the new holder owns it.
var ErrFenced = errors.New("run: fenced out; another worker holds this node")

// ErrRefused is a run the mill will not continue: an invariant the store
// could not reconcile, such as a done run on an open node.
var ErrRefused = errors.New("run: refused")

// Workspace is the run loop's view of a workspace: the verbs a step reaches
// through the socket, plus the two things the loop itself needs — the handle
// it records, and a git runner for the `commit` verifier.
type Workspace interface {
	workspace.Workspace
	// Handle is the workspace as the ledger records it.
	Handle() *workspace.Handle
	// GitRunner runs git in the checkout, through the same chokepoint.
	GitRunner() forge.Git
	// Committer is the identity the workspace's own commits carry. The
	// `commit` verifier compares a commit's author and committer against
	// it, and it comes from the workspace rather than from this loop's
	// options so that what is checked is the identity git was actually
	// given.
	Committer() workspace.Committer
	// Environment is what a process the workspace spawns must run under:
	// the run's git identity and the mill's signing key with no
	// configuration of the host account's, and the proxy and the model
	// gateway the workspace was given, with the current step token as the
	// gateway's bearer. The loop hands it to each step's session, after
	// the token is rotated, so that a session that runs `git commit` itself
	// makes the same commit the workspace would have and a session that
	// reaches out goes the way the workspace decided.
	Environment() map[string]string
}

// Workspaces makes the workspace a run works in: reattached when this worker
// left one behind, cloned from the run's branch when it did not.
type Workspaces func(ctx context.Context, spec workspace.Spec) (Workspace, error)

// Forges resolves the project's forge. It is a function rather than a value
// because a run that never reaches a `forge` step should never need one.
type Forges func(ctx context.Context) (forge.Forge, error)

// Options is everything the runner is built from. Every field but the clock
// and the log is required.
type Options struct {
	// Store is `mill serve`, over its unix socket.
	Store millv1connect.StoreServiceClient
	// Dagger is the work graph.
	Dagger *dagger.Client
	// Workspaces makes the run's workspace.
	Workspaces Workspaces
	// Forges resolves the project's forge.
	Forges Forges
	// Harnesses is the adapter table a skill step's harness name is looked
	// up in.
	Harnesses *harness.Registry
	// WorkerRef is this worker, recorded on the run and on every attempt.
	WorkerRef string
	// Workflow is the workflow name to select when a run has none. The
	// project default, until the assessment step exists.
	Workflow string
	// Origin is the repository a new workspace clones.
	Origin string
	// WorkflowDir is the directory the workflow was loaded from, which a
	// check step's relative command resolves against. Skill bodies come
	// from the store; the scripts beside them do not, because the store
	// keeps their digests and not their bytes.
	WorkflowDir string
	// Ref is what a new workspace checks out: the project's default branch.
	Ref string
	// TTL is the lease TTL asked for at every claim and heartbeat.
	TTL time.Duration
	// Interval is how often the lease is renewed. Zero takes a third of the
	// TTL, which is the bound docs/architecture.md sets on it.
	Interval time.Duration
	// Grace is how long a cancelled step has to stop.
	Grace time.Duration
	// Out is where the run's progress is written. `mill run` streams to the
	// terminal (docs/prd.md R-45).
	Out io.Writer
	// LogDir is where a step run's log file is written, one file per
	// attempt, through [Options.Redactor]. Empty keeps no logs, and
	// `mill logs` then has nothing to read.
	LogDir string
	// Redactor scrubs every step log. It is the workspace's own, so a value
	// a session registers with the `redact` verb during a step never reaches
	// the log an operator tails (docs/architecture.md §Observability).
	Redactor *redact.Redactor
	// Log is where diagnostics go. Nil discards them.
	Log *slog.Logger
	// Now is the clock. Nil means [time.Now].
	Now func() time.Time
	// Notifier carries a filed gate to a person, through whatever command
	// the deployment configured. Nil never notifies, which is a deployment
	// that has not configured one: the gate is still filed, and whoever
	// answers it finds it in dagger (docs/architecture.md §"Gate steps").
	Notifier notify.Notifier
	// Answers puts a gate's ask to a person and reports their decision.
	// Only single-run mode sets it: a worker files a gate and goes back to
	// ClaimNext, because there is nobody at its terminal. Nil never asks.
	Answers Answerer
	// SkillOverrides replaces a step's skill with one read from disk,
	// keyed by step name. It is empty in every production run: `mill run
	// --skill-override` is a test seam the acceptance suite uses to script
	// an implementer, refused unless MILL_ACCEPTANCE is set
	// (docs/runbook.md §"The scripted implementer").
	SkillOverrides map[string]*millv1.Skill
	// Tier is what this run is worth, which gate `auto` predicates compare
	// against. It is the project default's tier until an assessment step
	// exists to name one (docs/domain-model.md §"Workflow selection").
	Tier millv1.Tier
	// Egress is what every workspace this runner makes decides about the
	// way out: the proxy and the model gateway its sessions are pointed
	// at, or neither (workspace.Egress). The zero value is today's local
	// run — the host's own route and the account's login.
	Egress workspace.Egress
	// Gateway is the credential gateway the sessions' model requests go
	// through, when Egress names one: the loop installs each attempt's
	// token, budget and model pin in it beside the workspace's own
	// rotation, and reads a budget refusal off it when the session ends.
	// Nil means no gateway sits in front of the sessions.
	Gateway Gateway
}

// Gateway is the run loop's view of the credential gateway
// (internal/gateway): what it installs per attempt and what it reads back.
type Gateway interface {
	// Rotate installs the attempt whose token the run's requests carry
	// from now on; the one before it is refused at once.
	Rotate(runID string, step gateway.Step)
	// Revoke removes the run's attempt, so its token is refused.
	Revoke(runID string)
	// Exceeded is the refusal an attempt earned by spending its budget,
	// or nil.
	Exceeded(stepRunID string) *gateway.BudgetError
}

// Answerer is a person at a terminal, asked a gate's question. It is the
// only seam through which a run in this process learns a human's decision;
// every other run learns it by being claimed again after the gate closed.
type Answerer interface {
	// Answer puts the ask to the person and reports whether they approved.
	Answer(ctx context.Context, ask Ask) (bool, error)
}

// Ask is a filed gate as a person sees it.
type Ask struct {
	// NodeRef is the node the gate blocks.
	NodeRef string
	// GateRef is the gate node itself.
	GateRef string
	// Title and Body are the ask, already rendered.
	Title string
	// Body is the ask's body.
	Body string
}

// Runner drives one node's run.
type Runner struct {
	options Options
}

// New builds a runner, applying the defaults.
func New(options Options) *Runner {
	if options.TTL <= 0 {
		options.TTL = DefaultTTL
	}
	if options.Interval <= 0 || options.Interval > options.TTL/3 {
		options.Interval = options.TTL / 3
	}
	if options.Grace <= 0 {
		options.Grace = DefaultGrace
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Tier == millv1.Tier_TIER_UNSPECIFIED {
		options.Tier = millv1.Tier_TIER_QUICK_FIX
	}
	if options.Log == nil {
		options.Log = slog.New(slog.DiscardHandler)
	}
	if options.Out == nil {
		options.Out = io.Discard
	}
	if options.Redactor == nil {
		options.Redactor = redact.New()
	}
	return &Runner{options: options}
}

// Interval is how often the runner renews its lease. It is never more than
// a third of the TTL: docs/architecture.md's rule is that a worker renews
// well inside the window, so that one missed tick is not a lost node.
func (r *Runner) Interval() time.Duration { return r.options.Interval }

// state is one run in flight: what the loop carries between its stages.
type state struct {
	node      *daggerv1.Node
	lease     string
	run       *millv1.Run
	workflow  *millv1.Workflow
	workspace Workspace
	// beat is the heartbeat, which owns the step's context.
	beat *heartbeat
	// stopping is closed when the process the run is in has been asked to
	// stop. The heartbeat watches it alongside the lease, so a shutdown
	// reaches a step exactly as a cancellation does. Nil never fires.
	stopping <-chan struct{}
	// process is the context of the process the run is in, which a step
	// stopping never cancels. Only a kill does — a worker signalled a
	// second time inside its grace window (docs/architecture.md §"Failure
	// and stop"). It is what the work that has to outlive a cancelled step
	// runs under, and what says that work is no longer worth waiting for.
	process context.Context

	// summary is the implementer's last word, which becomes the node's
	// result body.
	summary string
	// pullRequestURL is the pull request the run opened, once a `url` step
	// has closed on it.
	pullRequestURL string
	// mergeCommit is the commit the branch became once a merge gate was
	// approved and the merge made.
	mergeCommit string
	// token is the step token issued for the attempt in flight, so a gate
	// step can revoke it before the run stops waiting on a human.
	token string
	// parked says this claim took over a run the circuit breaker parked, so
	// the park gate is read before any step runs.
	parked bool
	// findings is what each attempt at a step failed on, in order, so the
	// comment the circuit breaker leaves carries every round rather than
	// only the last. It is this worker's memory: a run resumed elsewhere
	// comments the rounds that worker saw.
	findings map[string][]string
	// retried is the attempt each step's retry count is measured from. A
	// human answering a park gate pays for a fresh set of attempts, and
	// this is where that is recorded (docs/architecture.md §"The circuit
	// breaker").
	retried map[string]int64
	// span is every attempt this worker made since the last one a human
	// paid for, oldest first and across every step, which is what the park
	// gate's body is written from (internal/run/breaker.go).
	span []attempt
	// boundary is the workspace's step boundary when it has one — the
	// machine backend, which runs each step under its own slice and reaps
	// the step by stopping it (docs/sandbox.md §"Step boundary"). Nil for a
	// backend with no boundary, such as the local one.
	boundary workspace.StepBoundary
	// ordinal counts the steps this worker has begun, so each gets a slice
	// of its own, `mill-step-<ordinal>.slice`. It advances per attempt: a
	// retry is a fresh slice, because the previous attempt's was reaped.
	ordinal int
}

// processContext is the context of the process the run is in.
func (s *state) processContext() context.Context {
	if s.process == nil {
		return context.Background()
	}
	return s.process
}

// record appends what one failed attempt at a step reported.
func (s *state) record(step, findings string) {
	if findings == "" {
		return
	}
	if s.findings == nil {
		s.findings = map[string][]string{}
	}
	s.findings[step] = append(s.findings[step], findings)
}

// attemptsAt is how many attempts at a step count against its retries: every
// attempt since the last one a human paid for.
func (s *state) attemptsAt(step string, attempt int64) int64 {
	return attempt - s.retried[step]
}

// Execute drives one node from a claim to a result. It is docs/ledger.md
// §Resume, and every stage below is a line of it.
func (r *Runner) Execute(ctx context.Context, nodeRef string) error {
	node, err := r.options.Dagger.Node(ctx, nodeRef)
	if err != nil {
		return err
	}
	if node.GetAssigneeKind() == daggerv1.AssigneeKind_ASSIGNEE_KIND_USER {
		return &dagger.UserNodeError{Ref: nodeRef, Verb: "claim"}
	}

	// A node already done in dagger is the crash window of Invariant 11: the
	// completing patch landed and store.Done did not. Nothing is claimed —
	// the node is not claimable and there is nothing left to do to it — and
	// the run is simply caught up with what dagger already says.
	if node.GetStatus() == daggerv1.Status_STATUS_DONE {
		return r.reconcileDone(ctx, node)
	}

	claimed, err := r.options.Dagger.Claim(ctx, nodeRef)
	if err != nil {
		return err
	}
	return r.drive(ctx, claimed, nil)
}

// ExecuteClaimed drives a node `ClaimNext` has already claimed, which is the
// only difference between the daemon and single-run mode: `mill work` is
// handed a node and its lease, `mill run` is handed a reference and takes
// the lease itself. Everything after the claim is this same function, which
// is what keeps single-run mode a real test of the daemon.
//
// stopping is closed when the process has been asked to shut down. The run
// then stops at the first boundary it can reach, aborts the attempt in
// flight, releases the lease `preempted`/`shutdown`, and stays resumable.
// Nil never fires, which is what `mill run` passes.
func (r *Runner) ExecuteClaimed(ctx context.Context, claimed *dagger.Claimed, stopping <-chan struct{}) error {
	return r.drive(ctx, claimed, stopping)
}

// drive is the run loop proper: everything from a held lease to a result.
func (r *Runner) drive(ctx context.Context, claimed *dagger.Claimed, stopping <-chan struct{}) error {
	s := &state{
		node:     claimed.Node,
		lease:    claimed.Lease.GetId(),
		retried:  map[string]int64{},
		stopping: stopping,
		process:  ctx,
	}
	r.printf("claimed %s under lease %s\n", s.node.GetReference(), s.lease)

	if err := r.openRun(ctx, s); err != nil {
		return r.stop(ctx, s, err)
	}

	s.beat = r.startHeartbeat(ctx, s)
	defer s.beat.stop()

	if err := r.openWorkspace(s.beat.ctx, s); err != nil {
		return r.stop(ctx, s, err)
	}
	if err := r.selectWorkflow(s.beat.ctx, s); err != nil {
		return r.stop(ctx, s, err)
	}
	if err := r.requireHarnesses(s); err != nil {
		return r.stop(ctx, s, err)
	}
	if err := r.resumePark(s.beat.ctx, s); err != nil {
		return r.stop(ctx, s, err)
	}
	return r.stop(ctx, s, r.walk(s.beat.ctx, s))
}

// reconcileDone marks a run done whose node dagger already says is done. The
// second `mill run` of the crash scenario ends here: it finds the node done,
// marks the run done, and sends no second patch.
func (r *Runner) reconcileDone(ctx context.Context, node *daggerv1.Node) error {
	run, err := r.runByNode(ctx, node.GetId())
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: %s is done in dagger and the ledger has no run for it", ErrRefused, node.GetReference())
	}
	if run.GetStatus() == millv1.RunStatus_RUN_STATUS_DONE {
		r.printf("%s is already done\n", node.GetReference())
		return nil
	}
	if _, err := call(ctx, r.options.Store.Done, &millv1.DoneRequest{
		LeaseId: run.GetLeaseId(),
		RunId:   run.GetId(),
	}); err != nil {
		return err
	}
	r.printf("%s was completed in dagger; run %s marked done\n", node.GetReference(), run.GetId())
	return nil
}

// openRun finds or creates the run and takes it over under this claim's
// lease. It is the switch at the head of the resume algorithm.
func (r *Runner) openRun(ctx context.Context, s *state) error {
	run, err := r.runByNode(ctx, s.node.GetId())
	if err != nil {
		return err
	}
	switch {
	case run == nil:
		created, err := call(ctx, r.options.Store.CreateRun, &millv1.CreateRunRequest{
			LeaseId:    s.lease,
			NodeId:     s.node.GetId(),
			NodeRef:    s.node.GetReference(),
			ContextId:  s.node.GetContextId(),
			Generation: 1,
			WorkerRef:  r.options.WorkerRef,
			Branch:     workspace.BranchFor(s.node.GetReference()),
		})
		if err != nil {
			return err
		}
		// CreateRun takes the lease as it opens the run, so there is
		// nothing to take over: SetLease is the write that changes what
		// later writes compare against, and this claim is already what
		// they compare against.
		s.run = created.GetRun()
		return nil

	case run.GetStatus() == millv1.RunStatus_RUN_STATUS_DONE:
		// The node is open — Execute returned above if it were not — so a
		// done run on it is Invariant 11 violated. Park rather than guess.
		return fmt.Errorf("%w: run %s is done but %s is open", ErrRefused, run.GetId(), s.node.GetReference())

	case run.GetStatus() == millv1.RunStatus_RUN_STATUS_FAILED:
		// A failed run is terminal to the loop: nothing mill does on its
		// own leaves it (docs/domain-model.md §"Run states"). A lost
		// sandbox leaves one on an open node, and a claim on that node is
		// refused rather than driven, until a person reruns or restarts.
		return fmt.Errorf("%w: run %s of %s failed; `mill restart` starts the next generation, `mill rerun` reopens this one",
			ErrRefused, run.GetId(), s.node.GetReference())

	default:
		// The circuit breaker parked this run and a human has been asked
		// what to do. Whether they answered is read after the workflow is
		// known, because the answer resumes the run at the parked step.
		s.parked = run.GetStatus() == millv1.RunStatus_RUN_STATUS_PARKED && run.GetGateNodeId() != ""

		// A `selecting` run is one whose worker died before it chose a
		// workflow — between CreateRun and the selection, which is where a
		// workspace is made. It is resumed like any other: the lease is
		// taken over below, and selectWorkflow chooses again. Selection is
		// idempotent — the same node, the same project default, the same
		// version — so choosing again is choosing the same thing, recorded
		// as a second attempt at the selection step.
		//
		// A `running` row under a `running` or `selecting` run belongs to a
		// worker that died or was fenced out, and the next attempt at that
		// step starts from the top. A `waiting` run's open row is not that:
		// it is the gate step's own attempt, left open on purpose so that
		// the answer closes the attempt that asked it
		// (docs/domain-model.md §Gate, "a gate is filed once"; the gate step
		// has one step run, not two).
		if run.GetStatus() == millv1.RunStatus_RUN_STATUS_RUNNING ||
			run.GetStatus() == millv1.RunStatus_RUN_STATUS_SELECTING {
			if _, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
				LeaseId: run.GetLeaseId(),
				RunId:   run.GetId(),
				Reason:  "the worker that held this run is gone",
			}); err != nil {
				return err
			}
		}
	}

	leased, err := call(ctx, r.options.Store.SetLease, &millv1.SetLeaseRequest{
		LeaseId:   s.lease,
		RunId:     run.GetId(),
		WorkerRef: r.options.WorkerRef,
	})
	if err != nil {
		return err
	}
	s.run = leased.GetRun()
	return nil
}

// openWorkspace reattaches the workspace this worker left behind, or clones
// one. The loader guarantees the branch is on the forge before any gate that
// follows a write, so a worker elsewhere can always recreate it.
func (r *Runner) openWorkspace(ctx context.Context, s *state) error {
	// A run that has not selected a workflow has run no step, so its branch
	// is not on the forge and there is nothing to clone but the default
	// ref. The workflow id says that and the status does not: the claim
	// that takes a run over has already marked it running, and a run
	// `mill restart` opened at the next generation starts from the default
	// branch exactly as the first generation did.
	ref := r.options.Ref
	if s.run.GetWorkflowId() != "" {
		ref = s.run.GetBranch()
	}
	ws, err := r.options.Workspaces(ctx, workspace.Spec{
		RunID:   s.run.GetId(),
		NodeRef: s.run.GetNodeRef(),
		Origin:  r.options.Origin,
		Ref:     ref,
		Egress:  r.options.Egress,
	})
	if errors.Is(err, workspace.ErrLostSandbox) {
		return r.lostSandbox(ctx, s, err)
	}
	if err != nil {
		return fmt.Errorf("run %s: workspace: %w", s.run.GetId(), err)
	}
	s.workspace = ws
	if boundary, ok := ws.(workspace.StepBoundary); ok {
		s.boundary = boundary
		if resumer, ok := ws.(workspace.StepResumer); ok {
			// The count continues from the workspace's: a slice a dead
			// worker left in flight is recreated under its own ordinal.
			s.ordinal = resumer.NextOrdinal() - 1
		}
	}
	return nil
}

// lostSandbox ends a run whose machine is gone: the ledger says the run is
// live, this host made a machine for it, and the machine is no longer
// registered. The run fails with that as evidence — the machine's name and
// the ledger's step — and nothing boots a fresh machine for it, because the
// workspace's state was the machine's and is not the ledger's to
// reconstruct (docs/sandbox.md §Reconciliation, §"Failure modes").
//
// The node is held in dagger under the same reason, so it leaves the ready
// set rather than being claimed again into the same refusal, and the person
// who restarts it finds why on the node. Then the lease is released.
func (r *Runner) lostSandbox(ctx context.Context, s *state, cause error) error {
	detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
	if err != nil {
		return err
	}
	step := detail.GetSummary().GetCurrentStep()
	if step == "" {
		step = "(selecting)"
	}
	reason := fmt.Sprintf("lost sandbox: %v; the ledger's step is %s", cause, step)
	if _, err := call(ctx, r.options.Store.Fail, &millv1.FailRequest{
		LeaseId: s.lease,
		RunId:   s.run.GetId(),
		Reason:  reason,
	}); err != nil {
		return err
	}
	if err := r.options.Dagger.Hold(ctx, dagger.RequestID(s.run.GetId(), "lost-sandbox", s.run.GetGeneration()),
		s.lease, s.node.GetReference(), reason); err != nil {
		r.options.Log.Warn("holding the node of a lost sandbox", "node", s.node.GetReference(), "error", err)
	}
	r.printf("%s: run %s failed: %s\n", s.node.GetReference(), s.run.GetId(), reason)
	return fmt.Errorf("run %s: %w", s.run.GetId(), cause)
}

// runByNode reads the live run on a node, or nil.
func (r *Runner) runByNode(ctx context.Context, nodeID string) (*millv1.Run, error) {
	res, err := call(ctx, r.options.Store.GetRunByNode, &millv1.GetRunByNodeRequest{NodeId: nodeID})
	if err != nil {
		return nil, err
	}
	return res.GetRun(), nil
}

// stop is the one exit. A fenced runner writes nothing more — not to the
// store, not to dagger — because everything it could write belongs to the
// worker that fenced it out. Every other failure gives the lease back so the
// node is claimable again rather than waiting out its TTL.
func (r *Runner) stop(ctx context.Context, s *state, err error) error {
	if r.options.Gateway != nil && s.run != nil {
		// Whatever the run had installed in the gateway dies with the
		// run's turn on this worker, however the turn ended.
		r.options.Gateway.Revoke(s.run.GetId())
	}
	if s.beat != nil {
		s.beat.stop()
		if beatErr := s.beat.err(); beatErr != nil && err == nil {
			err = beatErr
		}
	}
	if s.workspace != nil && err != nil {
		// A failed run keeps its workspace: the next attempt reattaches it,
		// and a human looking at what went wrong needs the tree.
		r.options.Log.Debug("workspace retained", "run", s.run.GetId())
	}
	if err == nil || errors.Is(err, ErrFenced) || errors.Is(err, errReleased) {
		if errors.Is(err, errReleased) {
			return nil
		}
		return err
	}
	if s.lease != "" {
		if releaseErr := r.options.Dagger.Release(ctx, s.lease, daggerv1.ReleaseReason_RELEASE_REASON_CRASH, err.Error()); releaseErr != nil {
			r.options.Log.Warn("releasing the lease after a failure", "error", releaseErr)
		}
	}
	return err
}

// errReleased ends the loop where the run is meant to stop and stay
// resumable: a gate filed, a park, or a cancellation. The lease is already
// released by the branch that raises it.
var errReleased = errors.New("run: released")

// printf writes one line of progress to the run's output.
func (r *Runner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.options.Out, format, args...)
}

// call is one Connect request against the store, unwrapped to its message
// and with a fenced refusal turned into [ErrFenced]. Every store write is
// fenced by the lease (Invariant 9), so every store call goes through here.
func call[Req, Res any](ctx context.Context, method func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req *Req) (*Res, error) {
	res, err := method(ctx, connect.NewRequest(req))
	if err != nil {
		if isFenced(err) {
			return nil, fmt.Errorf("%w: %w", ErrFenced, err)
		}
		return nil, err
	}
	return res.Msg, nil
}

// isFenced reports whether a failure is a lease that is no longer current,
// on either side. The store answers a fenced write with FAILED_PRECONDITION
// and a message naming both leases; dagger answers the same code with a
// message naming the lease. Both halves are checked, so a precondition of
// another kind is not mistaken for a fence.
func isFenced(err error) bool {
	if errors.Is(err, ErrFenced) || errors.Is(err, dagger.ErrFenced) {
		return true
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return false
	}
	if connectErr.Code() != connect.CodeFailedPrecondition {
		return false
	}
	return containsAny(connectErr.Message(), "fenced", "lease")
}
