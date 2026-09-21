package server

import (
	"context"

	"connectrpc.com/connect"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/doctor"
	"github.com/ivy/mill/internal/forge"
)

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

	return connect.NewResponse(&millv1.ProbeResponse{
		Sidecar: probeResultProto(sidecar),
		Signing: probeResultProto(signing),
		Forge:   probeResultProto(forgeResult),
	}), nil
}

func probeResultProto(r doctor.Result) *millv1.ProbeResult {
	return &millv1.ProbeResult{Ok: r.OK, Reason: r.Reason}
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
