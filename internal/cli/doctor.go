package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/doctor"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/harness/claude"
	"github.com/ivy/mill/internal/notify"
	"github.com/ivy/mill/internal/workflow"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// `mill doctor` is the one tool that asks whether the mill could work at
// all, rather than what it did. Each probe is one thing a run needs and one
// line of reason, so a failed probe names what to fix rather than what
// broke.
//
// A probe never fails the command by erroring: it reports. `mill doctor`
// exits non-zero when any probe failed, which is what a health check wants,
// and prints every probe either way, which is what a human wants.

// ProbeNames is every probe, in the order `mill doctor` reports them. The
// command's usage text and docs/tools.md §doctor name each of them, and a
// spec holds both to this list.
var ProbeNames = []string{
	"socket", "schema", "dagger", "sidecar", "signing",
	"forge", "notify", "workspace", "isolation", "space", "gateway", "mcp", "workflows",
}

// probe is one check and what it found.
type probe struct {
	// Name is what was checked.
	Name string `json:"name"`
	// OK is whether it passed.
	OK bool `json:"ok"`
	// Reason is the one line a human reads: what answered, or what did not.
	Reason string `json:"reason"`
	// Warning is what a passing probe still wants looked at: the gateway's
	// credential nearing its year. It never fails the command.
	Warning string `json:"warning,omitempty"`
}

// pass and fail build a probe's result.
func pass(name, format string, args ...any) probe {
	return probe{Name: name, OK: true, Reason: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) probe {
	return probe{Name: name, OK: false, Reason: fmt.Sprintf(format, args...)}
}

type doctorOptions struct {
	socket        string
	daggerSocket  string
	project       string
	sidecar       string
	envMode       string
	workspaceDir  string
	workspace     string
	sandboxRoot   string
	identity      string
	name          string
	forge         string
	config        string
	notifyCommand string
	workflowsRoot string
	gateway       gatewayOptions
	// credentialMinted is the configuration file's model_credential_minted,
	// which has no flag: it is a fact about a deployment's credential.
	credentialMinted string
	// highWatermark and lowWatermark are the eviction watermarks the
	// space probe reads free space against, from the configuration file;
	// zero is the default.
	highWatermark cache.Watermark
	lowWatermark  cache.Watermark
	// mcpServers is the host's own mcp-servers, from the configuration
	// file: file-only, like McpServers itself (no flag spelling).
	mcpServers map[string]McpServerConfig
}

func newDoctorCommand() *cobra.Command {
	opts := doctorOptions{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Probe everything a run needs and report each as pass or fail",
		Long: `Probe the things a run depends on, one line each:

  socket    the service answers on its unix socket
  schema    the database is migrated to the schema this binary carries
  dagger    dagger answers, and this account resolves to a principal there
  sidecar   the harness sidecar starts, under the environment mode a run
            would spawn it with, and answers a no-op
  signing   the commit signing key exists and some project trusts it
  forge     the forge accepts the credential, and says which account it
            is: a token that has expired, or lacks a permission the
            repository needs, fails here naming the credential
  notify    the configured notify command runs and accepts a probe notice
  workspace the workspace kind in effect and, for machine, whether the
            sandbox root is btrfs
  isolation a session's own tools see dead paths for DG_SOCKET and
            MILL_SOCKET, answering no dial, never the production dagger
            or mill socket this host actually serves
  space     free space on the sandbox volume against the eviction
            watermarks: a warning below the low one, a failure below the
            high one
  gateway   the credential gateway's upstream answers one token count
            through it, and a warning once the credential is eleven
            months old
  mcp       each configured mcp-servers entry starts, answers initialize
            and tools/list, and is stopped: one line per server, naming
            the tool count or what kept it from answering
  workflows each workflow directory under --workflows-root against what
            the store holds: one line per workflow, naming the two
            versions when they disagree

Every probe is reported. The command exits non-zero when any of them
failed; a warning does not fail it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	f.StringVar(&opts.daggerSocket, "dagger-socket", envOr(daggerSocketEnv, defaultDaggerSocket), "path of dagger's unix socket (default: $"+daggerSocketEnv+")")
	f.StringVar(&opts.project, "project", os.Getenv(daggerProjectEnv), "dagger project to probe readiness in (default: $"+daggerProjectEnv+")")
	f.StringVar(&opts.sidecar, "sidecar", envOr(sidecarEnv, claude.DefaultEntrypoint), "path of the harness sidecar's entrypoint (default: $"+sidecarEnv+")")
	f.StringVar(&opts.envMode, "env-mode", string(claude.DefaultEnvMode), envModeUsage)
	f.StringVar(&opts.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces and the signing key live under")
	f.StringVar(&opts.workspace, "workspace", string(workspace.KindLocal), workspaceUsage)
	f.StringVar(&opts.sandboxRoot, "sandbox-root", machine.DefaultRoot, sandboxRootUsage)
	f.StringVar(&opts.identity, "identity", "", "address the mill commits under (default: mill@<host>)")
	f.StringVar(&opts.name, "name", defaultForgeUser, "name on the mill's commits")
	f.StringVar(&opts.forge, "forge", "", forgeUsage)
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file the notify command, the workspace kind and directory, the sidecar, the environment mode and the forge are read from when no flag names them")
	f.StringVar(&opts.notifyCommand, "notify-command", "", notifyCommandUsage)
	f.StringVar(&opts.workflowsRoot, "workflows-root", "", "directory of workflow subdirectories to check against the store (default: none, meaning nothing is checked)")
	bindGatewayFlags(cmd, &opts.gateway)
	return cmd
}

// ErrUnhealthy is what `mill doctor` returns when a probe failed. It carries
// no message of its own: every probe has already printed its line.
var ErrUnhealthy = errors.New("doctor: a probe failed")

func runDoctor(cmd *cobra.Command, opts doctorOptions) error {
	if opts.identity == "" {
		opts.identity = defaultIdentity()
	}
	// The notify command and the workspace kind are a deployment's settings
	// and live in its configuration file; a doctor that probed only what
	// the command line repeated would report on a notifier no run uses and
	// a workspace no run works in. The sidecar, the environment mode and
	// the workspace directory likewise: a packaged host names its installed
	// sidecar and its state directory in the file and nowhere else, so the
	// defaults would be a sidecar no run spawns and a signing key no run
	// signs with.
	config, err := LoadConfig(opts.config)
	if err != nil {
		return err
	}
	if opts.notifyCommand == "" && unset(cmd, "notify-command") {
		opts.notifyCommand = config.NotifyCommand
	}
	if config.Workspace != "" && unset(cmd, "workspace") {
		opts.workspace = config.Workspace
	}
	if config.Sidecar != "" && unset(cmd, "sidecar") {
		opts.sidecar = config.Sidecar
	}
	if config.EnvMode != "" && unset(cmd, "env-mode") {
		opts.envMode = config.EnvMode
	}
	if config.WorkspaceDir != "" && unset(cmd, "workspace-dir") {
		opts.workspaceDir = config.WorkspaceDir
	}
	if config.Forge != "" && unset(cmd, "forge") {
		opts.forge = config.Forge
	}
	if config.WorkflowsRoot != "" && unset(cmd, "workflows-root") {
		opts.workflowsRoot = config.WorkflowsRoot
	}
	// opts.project's flag default is already $DG_PROJECT (newDoctorCommand
	// declares it that way), so empty here means neither the flag nor the
	// variable supplied one — the file's project is read last, same as the
	// socket resolver's precedence (sockets.go).
	if opts.project == "" && unset(cmd, "project") && len(config.Projects) > 0 {
		opts.project = config.Projects[0]
	}
	if err := resolveSandboxRoot(cmd, &opts.sandboxRoot, config.SandboxRoot); err != nil {
		return err
	}
	applyGatewayConfig(cmd, config, &opts.gateway)
	opts.credentialMinted = config.ModelCredentialMinted
	opts.mcpServers = config.McpServers
	var expiry time.Duration
	if err := applyEvictionConfig(config, &expiry, &opts.highWatermark, &opts.lowWatermark); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "claims from: %s\n", claimsFrom(config.Projects))
	probes := runProbes(cmd.Context(), opts)
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	healthy := true
	for _, result := range probes {
		mark, reason := "pass", result.Reason
		switch {
		case !result.OK:
			mark, healthy = "fail", false
		case result.Warning != "":
			mark, reason = "warn", result.Reason+"; "+result.Warning
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", mark, result.Name, reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !healthy {
		return ErrUnhealthy
	}
	return nil
}

// runProbes runs every probe in order and returns what each found. It is
// the value `mill mcp` renders too, so an agent asked to look at a sick
// host is told the same nine things.
func runProbes(ctx context.Context, opts doctorOptions) []probe {
	socket, schema := probeService(ctx, opts)
	worker, workerErr := workerProbe(ctx, opts.socket)
	probes := []probe{
		socket,
		schema,
		probeDagger(ctx, opts),
		probeSidecar(ctx, opts, worker, workerErr),
		probeSigning(ctx, opts, worker, workerErr),
		probeForge(ctx, opts, worker, workerErr),
		probeNotify(ctx, opts),
		probeWorkspace(opts, driver.NewSystemd(workspace.Spawner{}, driver.Commands{})),
		probeIsolation(ctx, opts),
		probeSpace(opts, driver.NewSystemd(workspace.Spawner{}, driver.Commands{})),
		probeGateway(ctx, opts, time.Now()),
	}
	probes = append(probes, probeMcp(ctx, opts)...)
	return append(probes, probeWorkflows(ctx, opts)...)
}

// probeWorkflows checks every workflow directory under --workflows-root
// against what the store holds: read-only, so a diagnostic never mutates
// what it is reporting on and never hides drift by fixing it first. A host
// with none configured passes, the way a host with no mcp-servers does.
func probeWorkflows(ctx context.Context, opts doctorOptions) []probe {
	if opts.workflowsRoot == "" {
		return []probe{pass("workflows", "no workflows-root is configured")}
	}
	entries, err := os.ReadDir(opts.workflowsRoot)
	if err != nil {
		return []probe{fail("workflows", "%s: %v", opts.workflowsRoot, err)}
	}
	var probes []probe
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(opts.workflowsRoot, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, workflow.DefinitionFile)); err != nil {
			continue
		}
		probes = append(probes, probeWorkflowDir(ctx, opts, dir))
	}
	if len(probes) == 0 {
		return []probe{pass("workflows", "%s: no workflow directories found", opts.workflowsRoot)}
	}
	return probes
}

// probeWorkflowDir loads one workflow directory — the version Load computes
// is the same hash `mill workflow push` would store, since the store
// computes it the same way (internal/workflow's own doc comment) — and
// compares it against the newest version the store holds for that name.
func probeWorkflowDir(ctx context.Context, opts doctorOptions, dir string) probe {
	definition, _, err := workflow.Load(dir)
	if err != nil {
		return fail("workflows", "%s: %v", dir, err)
	}
	found, err := storeClient(opts.socket).GetWorkflow(ctx, connect.NewRequest(&millv1.GetWorkflowRequest{
		Name: definition.GetName(), Scope: definition.GetScope(), Project: definition.GetProject(),
	}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return fail("workflows", "%s: disk is %s, not stored", definition.GetName(), short(definition.GetVersion()))
	}
	if err != nil {
		return fail("workflows", "%s: %v", definition.GetName(), err)
	}
	stored := found.Msg.GetWorkflow()
	if stored.GetVersion() != definition.GetVersion() {
		return fail("workflows", "%s: store has %s, disk is %s — push required",
			definition.GetName(), short(stored.GetVersion()), short(definition.GetVersion()))
	}
	return pass("workflows", "%s: store matches disk at %s", definition.GetName(), short(definition.GetVersion()))
}

// probeMcp starts every configured mcp-servers entry, in name order so two
// runs report them the same way, and reports one line each. A host with
// none configured reports a single line saying so, rather than one for an
// empty set.
func probeMcp(ctx context.Context, opts doctorOptions) []probe {
	if len(opts.mcpServers) == 0 {
		return []probe{pass("mcp", "no mcp-servers are configured")}
	}
	names := make([]string, 0, len(opts.mcpServers))
	for name := range opts.mcpServers {
		names = append(names, name)
	}
	sort.Strings(names)
	probes := make([]probe, 0, len(names))
	for _, name := range names {
		server := opts.mcpServers[name]
		result := doctor.Mcp(ctx, name, server.Command, server.Args, server.Env)
		probes = append(probes, probe{Name: "mcp", OK: result.OK, Reason: result.Reason})
	}
	return probes
}

// workerProbe asks the worker behind opts.socket to run the sidecar,
// signing and forge probes itself, so a client reaching a worker over a
// forwarded socket is told the worker's own state rather than whatever
// files happen to sit on the caller's host (docs/architecture.md §"One
// binary, two faces"). This also covers doctor running on the worker's own
// host: it gets the same RPC answer a remote client would, which is the
// more authoritative one since it reads --work's actual configuration
// rather than doctor's own possibly-drifted flags.
//
// A (nil, nil) return means no worker answered this socket —
// CodeUnimplemented or CodeNotFound, an old server or a bare `mill serve`
// with no --work, where the RPC is simply not mounted — and the caller
// should fall back to probing this host directly. Any other error is the
// worker's own answer: it ran and could not be reached or refused, and that
// failure is reported verbatim rather than papered over by a local probe
// that would answer a different question.
func workerProbe(ctx context.Context, socket string) (*millv1.ProbeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	res, err := workerClient(socket).Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
	switch connect.CodeOf(err) {
	case connect.CodeUnimplemented, connect.CodeNotFound:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// fromWorkerOr reports the worker's own verdict for name — its answer
// verbatim, or its unreachability as a failure — when workerProbe found one;
// ok is false when no worker answered this socket and the caller should
// probe its own host instead, per workerProbe's doc comment.
func fromWorkerOr(name string, opts doctorOptions, worker *millv1.ProbeResponse, workerErr error, get func(*millv1.ProbeResponse) *millv1.ProbeResult) (result probe, ok bool) {
	if workerErr != nil {
		return fail(name, "%s: %v", opts.socket, workerErr), true
	}
	if worker != nil {
		r := get(worker)
		return probe{Name: name, OK: r.GetOk(), Reason: r.GetReason()}, true
	}
	return probe{}, false
}

// probeGateway makes one cheap request through the credential gateway a run
// would put its sessions behind: a token count, which the upstream answers
// without billing. What it proves is the whole of the path a session's
// request takes — the credential file is there, the gateway accepts a step
// token, the upstream accepts the swapped credential. What it adds is the
// one warning a setup-token gets: the CLI's own expiry warning is for
// `/login` credentials, so the age is read off the configuration file's
// model_credential_minted and reported at eleven months
// (docs/sandbox.md §Credentials).
//
// A host with no gateway configured passes: its sessions run on the
// account's login, which is what a run on it does.
func probeGateway(ctx context.Context, opts doctorOptions, now time.Time) probe {
	if !opts.gateway.enabled {
		return pass("gateway", "no gateway is configured; sessions reach the model on the account's login")
	}
	credential, err := gateway.LoadCredential(opts.gateway.credentialFile)
	if err != nil {
		return fail("gateway", "%v", err)
	}
	g, err := gateway.New(gateway.Config{Upstream: opts.gateway.upstream, Credential: credential})
	if err != nil {
		return fail("gateway", "%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	if err := g.Probe(ctx); err != nil {
		return fail("gateway", "%v", err)
	}
	result := pass("gateway", "%s answers a token count through the gateway with %s", opts.gateway.upstream, credential)
	result.Warning = credentialWarning(opts.credentialMinted, now)
	return result
}

// probeService asks the service about itself: one call answers both whether
// the socket is there and whether the schema is current.
func probeService(ctx context.Context, opts doctorOptions) (socket, schema probe) {
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	res, err := storeClient(opts.socket).Health(ctx, connect.NewRequest(&millv1.HealthRequest{}))
	if err != nil {
		return fail("socket", "%s did not answer: %v", opts.socket, err),
			fail("schema", "not asked: the service did not answer")
	}
	health := res.Msg
	socket = pass("socket", "%s answers; mill %s", opts.socket, health.GetVersion())
	switch {
	case !health.GetDatabase():
		schema = fail("schema", "the service cannot reach its database: %s", health.GetDetail())
	case health.GetMigrationVersion() != health.GetMigrationWanted():
		schema = fail("schema", "database is at migration %d; this binary carries %d",
			health.GetMigrationVersion(), health.GetMigrationWanted())
	default:
		schema = pass("schema", "migration %d, current", health.GetMigrationVersion())
	}
	return socket, schema
}

// probeDagger asks dagger for the ready set of the configured project. A
// successful answer proves two things at once: dagger is reachable on the
// socket, and the account this process runs as resolves to a principal that
// may read the project — dagger resolves a principal from the transport and
// refuses a uid it does not know (dagger's docs/architecture.md, the trust
// boundary).
//
// What it cannot prove is the principal's name. Dagger's API exposes no
// whoami and its principals carry no name, so "the principal is mill" is not
// a question dagger will answer; the reason line says what was checked.
func probeDagger(ctx context.Context, opts doctorOptions) probe {
	if opts.project == "" {
		return fail("dagger", "no project to probe: pass --project or set %s", daggerProjectEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	nodes, err := dagger.Dial(opts.daggerSocket, opts.project).Ready(ctx, []string{opts.project}, daggerv1.AssigneeKind_ASSIGNEE_KIND_AGENT)
	if err != nil {
		return fail("dagger", "%s did not answer as this account: %v", opts.daggerSocket, err)
	}
	return pass("dagger", "%s answers; this account is a principal on %s, %d node(s) ready for an agent",
		opts.daggerSocket, opts.project, len(nodes))
}

// probeSidecar starts the harness sidecar and asks it for a no-op — the
// logic lives in internal/doctor, so internal/server's WorkerService can
// answer the same question from the worker's own process. When a worker
// answered workerProbe, its answer is reported instead of probing this
// host: see workerProbe's doc comment.
func probeSidecar(ctx context.Context, opts doctorOptions, worker *millv1.ProbeResponse, workerErr error) probe {
	if result, ok := fromWorkerOr("sidecar", opts, worker, workerErr, (*millv1.ProbeResponse).GetSidecar); ok {
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	result := doctor.Sidecar(ctx, opts.sidecar, opts.envMode)
	return probe{Name: "sidecar", OK: result.OK, Reason: result.Reason}
}

// probeSigning finds the mill's signing key and looks for its public key in
// an allowed_signers file of a project the mill has checked out, falling
// back to the project's repository when a fresh host has no checkout yet —
// the logic lives in internal/doctor, so internal/server's WorkerService can
// answer the same question from the worker's own process. When a worker
// answered workerProbe, its answer is reported instead of probing this
// host: see workerProbe's doc comment.
func probeSigning(ctx context.Context, opts doctorOptions, worker *millv1.ProbeResponse, workerErr error) probe {
	if result, ok := fromWorkerOr("signing", opts, worker, workerErr, (*millv1.ProbeResponse).GetSigning); ok {
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	result := doctor.Signing(ctx, opts.workspaceDir, opts.name, opts.identity, signingContentReader(ctx, opts))
	return probe{Name: "signing", OK: result.OK, Reason: result.Reason}
}

// signingContentReader resolves the project's forge for the signing probe's
// repository fallback, the same way probeForge resolves it for its own
// probe. A project or forge that cannot be resolved simply means the
// fallback does not fire — that failure is probeForge's to report, not
// signing's.
func signingContentReader(ctx context.Context, opts doctorOptions) forge.ContentReader {
	if opts.project == "" {
		return nil
	}
	f, _, err := resolveForgeProject(ctx, opts.daggerSocket, opts.project, opts.forge)
	if err != nil {
		return nil
	}
	reader, ok := f.(forge.ContentReader)
	if !ok {
		return nil
	}
	return reader
}

// probeForge asks the forge whether the credential the mill holds — the
// credential store's forge-token, else $MILL_FORGE_TOKEN — will serve a
// delivery, before a run spends a session finding out. The repository is
// the project's, read from dagger the way a run reads it, so the probe
// needs the project and nothing else; the adapter is the one a run would
// use, resolved the same way, so a `forge:` that names no adapter fails
// here too.
//
// A forge that can say who the credential is (forge.Prober) is asked, and
// the account it names is on the line; one that cannot is asked about a
// branch nothing has ever pushed, and "no pull request" is it answering.
// Every line names where the credential came from: a passing one so an
// operator can see the store is in use, a failing one because the
// credential is what to replace.
//
// When a worker answered workerProbe, its answer is reported instead of
// resolving the project and credential on this host: see workerProbe's doc
// comment.
func probeForge(ctx context.Context, opts doctorOptions, worker *millv1.ProbeResponse, workerErr error) probe {
	if result, ok := fromWorkerOr("forge", opts, worker, workerErr, (*millv1.ProbeResponse).GetForge); ok {
		return result
	}
	if opts.project == "" {
		return fail("forge", "no project to read the repository from: pass --project or set %s", daggerProjectEnv)
	}
	token, err := loadForgeToken()
	if err != nil {
		return fail("forge", "%v", err)
	}
	if token.value == "" {
		return fail("forge", "the mill holds no forge credential: deliver %s to the credential store or set $%s",
			forgeCredentialName, forgeTokenEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	f, repositoryURL, err := resolveForgeProject(ctx, opts.daggerSocket, opts.project, opts.forge)
	if err != nil {
		return fail("forge", "%v", err)
	}
	result := doctor.Forge(ctx, f, token.source, repositoryURL)
	return probe{Name: "forge", OK: result.OK, Reason: result.Reason}
}

// probeNotify runs the configured notify command with a probe notice. It is
// the one probe whose subject is somebody else's script: what it proves is
// that the command exists, starts, reads the notice, and exits zero — which
// is the whole of the contract internal/notify holds it to.
//
// The notice carries `kind: probe` and no gate, so a notifier that routes on
// kind can tell a health check from a person's decision and stay quiet
// (contrib/notify/thicket-send.sh).
//
// A deployment with no notify command passes. Filing a gate that reaches
// nobody is a choice the mill supports — the answer is written in dagger
// either way — and a probe that failed for it would make every mill without
// a notifier report itself sick.
func probeNotify(ctx context.Context, opts doctorOptions) probe {
	command := notify.Parse(opts.notifyCommand)
	if command == nil {
		return pass("notify", "no notify command is configured; a filed gate waits in dagger for somebody to find it")
	}
	ctx, cancel := context.WithTimeout(ctx, notify.Timeout)
	defer cancel()
	result, err := command.Notify(ctx, notify.Notice{Kind: notify.KindProbe})
	if err != nil {
		return fail("notify", "%s could not be run: %v", opts.notifyCommand, err)
	}
	if !result.OK() {
		return fail("notify", "%s refused the probe notice: %s", opts.notifyCommand, result)
	}
	return pass("notify", "%s accepts a probe notice", opts.notifyCommand)
}

// probeWorkspace says which workspace backend a run on this host works in,
// and for the machine backend whether the sandbox root is on btrfs — the
// one fact the backend cannot do without, because ephemeral roots, cache
// lineages and eviction are all subvolume operations (docs/sandbox.md
// §Storage). The local backend has nothing to probe beyond naming itself.
//
// The driver is the seam the backend reaches the host through, so what the
// probe asks is what a run would ask.
func probeWorkspace(opts doctorOptions, host driver.Driver) probe {
	kind, err := workspace.ParseKind(opts.workspace)
	if err != nil {
		return fail("workspace", "%v", err)
	}
	if kind != workspace.KindMachine {
		return pass("workspace", "%s: a clone under the mill's account", kind)
	}
	fs, err := host.Statfs(opts.sandboxRoot)
	if err != nil {
		return fail("workspace", "%s: sandbox root %s: %v", kind, opts.sandboxRoot, err)
	}
	if !fs.Btrfs() {
		return fail("workspace", "%s: sandbox root %s is %s, not %s (docs/sandbox.md §Storage)",
			kind, opts.sandboxRoot, fs.Type, driver.TypeBtrfs)
	}
	return pass("workspace", "%s: sandbox root %s is %s", kind, opts.sandboxRoot, fs.Type)
}

// probeIsolation asks whether a session's own tools would ever see the
// production dagger or mill socket: the logic lives in internal/doctor, so
// internal/server's WorkerService can answer the same question from the
// worker's own process. It is local-only — no worker RPC round-trip,
// following probeWorkspace/probeSpace's own pattern — because a session's
// environment is a fact about this host's configuration and code, not about
// a run in flight.
func probeIsolation(ctx context.Context, opts doctorOptions) probe {
	ctx, cancel := context.WithTimeout(ctx, doctor.ProbeTimeout)
	defer cancel()
	result := doctor.SessionSockets(ctx, opts.workspaceDir, opts.socket, opts.daggerSocket)
	return probe{Name: "isolation", OK: result.OK, Reason: result.Reason}
}

// probeSpace reads free space on the sandbox volume against the eviction
// watermarks (docs/sandbox.md §Eviction): below the high one eviction runs
// at every trigger and the host is short of space, which fails; between
// the two it has been evicting, which warns; above the low one is health.
// The local backend has no sandbox volume and nothing to watch.
func probeSpace(opts doctorOptions, host driver.Driver) probe {
	kind, err := workspace.ParseKind(opts.workspace)
	if err != nil {
		return fail("space", "%v", err)
	}
	if kind != workspace.KindMachine {
		return pass("space", "%s: no sandbox volume to watch", kind)
	}
	fs, err := host.Statfs(opts.sandboxRoot)
	if err != nil {
		return fail("space", "sandbox root %s: %v", opts.sandboxRoot, err)
	}
	high, low := opts.highWatermark, opts.lowWatermark
	if high.IsZero() {
		high = cache.DefaultHighWatermark
	}
	if low.IsZero() {
		low = cache.DefaultLowWatermark
	}
	highBytes, lowBytes := high.Of(fs), low.Of(fs)
	if lowBytes < highBytes {
		return fail("space", "the low watermark %s is below the high watermark %s; eviction would stop before it started", low, high)
	}
	free := cache.FormatSize(fs.Free)
	summary := fmt.Sprintf("%s free of %s on %s; watermarks high %s (%s), low %s (%s)",
		free, cache.FormatSize(fs.Total), opts.sandboxRoot, high, cache.FormatSize(highBytes), low, cache.FormatSize(lowBytes))
	switch {
	case fs.Free < highBytes:
		return fail("space", "%s: below the high watermark, eviction runs at every promotion", summary)
	case fs.Free < lowBytes:
		result := pass("space", "%s", summary)
		result.Warning = "below the low watermark: eviction has been running"
		return result
	default:
		return pass("space", "%s", summary)
	}
}
