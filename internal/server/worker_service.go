package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
	"github.com/steading-ai/mill/internal/doctor"
	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/run"
)

// WorkerStore is what Probe needs to resolve one in-flight node to its
// current step: domain.Service's own Inspect, so this costs one in-process
// call per node rather than a round trip through Connect.
type WorkerStore interface {
	Inspect(ctx context.Context, p domain.InspectParams) (*domain.RunDetail, error)
}

// WorkerServiceConfig is the worker's own settings, resolved once by
// internal/cli/serve.go from --work's flags and configuration file — the
// same settings mill work already runs with — so WorkerService.Probe
// answers the sidecar, signing and forge probes from this process, not from
// whatever files happen to sit on the caller's host.
type WorkerServiceConfig struct {
	// SidecarEntrypoint and EnvMode are the harness sidecar's own settings.
	SidecarEntrypoint string
	EnvMode           string
	// WorkspaceDir, Name and Identity are the signing key's.
	WorkspaceDir string
	Name         string
	Identity     string
	// Forge resolves the project's forge adapter, the credential's source
	// line, and the repository URL — mill run's own resolution
	// (internal/cli/run.go's forges/forgeProject/forgeMinter/loadForgeToken),
	// called fresh on every probe so a rotated credential is the one
	// reported. Nil means no project is configured to probe against; the
	// forge probe fails naming that, sidecar and signing still answer.
	Forge func(ctx context.Context) (adapter forge.Forge, tokenSource, repositoryURL string, err error)
	// Host, Projects and Labels are Claims's whole answer: the worker's OS
	// hostname and its already-resolved claim filter, held in memory rather
	// than probed, so a client can ask on every status/doctor/run invocation
	// at no cost.
	Host     string
	Projects []string
	Labels   []string
	// Drain asks the local worker to stop claiming and drain every run in
	// flight to its next boundary.
	Drain func()
	// State and InFlight report the local worker's own state for Probe.
	State    func() millv1.WorkerState
	InFlight func() []run.RunningNode
	// Outage reports the upstream refusal the local worker is waiting out,
	// or nil, for Probe's UpstreamOutage field (mill#63). Nil func reports
	// none, the same as a worker with no gateway in front of it.
	Outage func() *gateway.UpstreamRefusal
	// Store resolves an in-flight node's current step for Probe, by its
	// node ref.
	//
	// Drain, State, InFlight, and Store are always set together: internal/cli/
	// serve.go only constructs a WorkerService at all when a worker runs in
	// this process (opts.work), and sets all four from that one worker. A bare
	// `serve` mounts no WorkerService, so WorkerService.Drain's nil check below
	// exists to keep this config constructible with its zero value in tests,
	// not because production ever leaves one of the four unset while running.
	Store WorkerStore
}

// WorkerService answers WorkerService.Probe from this process's own worker
// configuration — the three things only the process actually holding the
// sidecar entrypoint, the signing key and the forge credential can answer
// honestly. See proto/mill/v1/store.proto's WorkerService doc and
// docs/architecture.md §"One binary, two faces".
type WorkerService struct {
	cfg WorkerServiceConfig
}

// NewWorkerService builds the handler `mill serve --work` mounts.
func NewWorkerService(cfg WorkerServiceConfig) *WorkerService { return &WorkerService{cfg: cfg} }

var _ millv1connect.WorkerServiceHandler = (*WorkerService)(nil)

// Probe runs the sidecar, signing and forge probes and returns all three.
func (s *WorkerService) Probe(ctx context.Context, _ *connect.Request[millv1.ProbeRequest]) (*connect.Response[millv1.ProbeResponse], error) {
	var (
		adapter                    forge.Forge
		tokenSource, repositoryURL string
		forgeErr                   error
	)
	if s.cfg.Forge != nil {
		forgeCtx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
		adapter, tokenSource, repositoryURL, forgeErr = s.cfg.Forge(forgeCtx)
		cancel()
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	sidecar := doctor.Sidecar(sidecarCtx, s.cfg.SidecarEntrypoint, s.cfg.EnvMode)
	cancel()

	var reader forge.ContentReader
	if forgeErr == nil {
		reader, _ = adapter.(forge.ContentReader)
	}
	signingCtx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	signing := doctor.Signing(signingCtx, s.cfg.WorkspaceDir, s.cfg.Name, s.cfg.Identity, reader)
	cancel()

	var forgeResult doctor.Result
	switch {
	case s.cfg.Forge == nil:
		forgeResult = doctor.Result{Reason: "no project configured to read a repository from"}
	case forgeErr != nil:
		forgeResult = doctor.Result{Reason: forgeErr.Error()}
	default:
		forgeCtx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
		forgeResult = doctor.Forge(forgeCtx, adapter, tokenSource, repositoryURL)
		cancel()
	}

	var state millv1.WorkerState
	var inFlight []*millv1.InFlightRun
	if s.cfg.State != nil {
		state = s.cfg.State()
	}
	if s.cfg.InFlight != nil {
		for _, n := range s.cfg.InFlight() {
			inFlight = append(inFlight, s.inFlightRun(ctx, n))
		}
	}
	var outage *millv1.UpstreamOutage
	if state == millv1.WorkerState_WORKER_STATE_WAITING_ON_UPSTREAM && s.cfg.Outage != nil {
		outage = upstreamOutageProto(s.cfg.Outage())
	}

	return connect.NewResponse(&millv1.ProbeResponse{
		Sidecar:        probeResultProto(sidecar),
		Signing:        probeResultProto(signing),
		Forge:          probeResultProto(forgeResult),
		State:          state,
		InFlight:       inFlight,
		UpstreamOutage: outage,
	}), nil
}

// upstreamOutageProto reports the outage a waiting worker is watching, or
// nil when it cleared between State and this read.
func upstreamOutageProto(o *gateway.UpstreamRefusal) *millv1.UpstreamOutage {
	if o == nil {
		return nil
	}
	out := &millv1.UpstreamOutage{Since: timestamppb.New(o.Since)}
	if !o.Reset.IsZero() {
		out.ResetAt = timestamppb.New(o.Reset)
	}
	return out
}

// inFlightRun looks up one in-flight node's current step, tolerating a
// lookup that fails: the node ref and a zero step are still worth reporting,
// the same tolerance resolvedClaims already gives a failed Claims call.
func (s *WorkerService) inFlightRun(ctx context.Context, n run.RunningNode) *millv1.InFlightRun {
	out := &millv1.InFlightRun{NodeRef: n.NodeRef}
	if s.cfg.Store == nil {
		return out
	}
	detail, err := s.cfg.Store.Inspect(ctx, domain.InspectParams{NodeRef: n.NodeRef})
	if err != nil || detail.Summary == nil {
		return out
	}
	out.RunId = detail.Summary.GetRun().GetId()
	out.CurrentStep = detail.Summary.GetCurrentStep()
	latest := latestStepRun(detail, out.CurrentStep, 0)
	if started := latest.GetStartedAt(); started != nil {
		out.TimeInStep = durationpb.New(time.Since(started.AsTime()))
	}
	return out
}

func probeResultProto(r doctor.Result) *millv1.ProbeResult {
	return &millv1.ProbeResult{Ok: r.OK, Reason: r.Reason}
}

// Drain asks the local worker to stop claiming and drain every run in flight
// to its next boundary, with no attempt aborted. It is fire-and-forget:
// `mill drain --wait` polls Probe for the state this leads to, rather than
// this call blocking for it.
func (s *WorkerService) Drain(_ context.Context, _ *connect.Request[millv1.DrainRequest]) (*connect.Response[millv1.DrainResponse], error) {
	if s.cfg.Drain == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no worker runs in this process"))
	}
	s.cfg.Drain()
	return connect.NewResponse(&millv1.DrainResponse{}), nil
}

// Claims answers the worker's own already-resolved claim filter and host:
// no probe, no side effect, so a caller can afford this on every status,
// doctor or run invocation.
func (s *WorkerService) Claims(_ context.Context, _ *connect.Request[millv1.ClaimsRequest]) (*connect.Response[millv1.ClaimsResponse], error) {
	return connect.NewResponse(&millv1.ClaimsResponse{
		Host:     s.cfg.Host,
		Projects: s.cfg.Projects,
		Labels:   s.cfg.Labels,
	}), nil
}
