package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/harness/claude"
	"github.com/ivy/mill/internal/notify"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/signing"
)

// `mill doctor` is the one tool that asks whether the mill could work at
// all, rather than what it did. Each probe is one thing a run needs and one
// line of reason, so a failed probe names what to fix rather than what
// broke.
//
// A probe never fails the command by erroring: it reports. `mill doctor`
// exits non-zero when any probe failed, which is what a health check wants,
// and prints every probe either way, which is what a human wants.

// probeTimeout bounds one probe. A probe that hangs is a failed probe: the
// point is to answer, not to wait.
const probeTimeout = 10 * time.Second

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
	vcsURL        string
	identity      string
	name          string
	config        string
	notifyCommand string
	gateway       gatewayOptions
	// credentialMinted is the configuration file's model_credential_minted,
	// which has no flag: it is a fact about a deployment's credential.
	credentialMinted string
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
  forge     the forge credential is accepted
  notify    the configured notify command runs and accepts a probe notice
  workspace the workspace kind in effect and, for machine, whether the
            sandbox root is btrfs
  gateway   the credential gateway's upstream answers one token count
            through it, and a warning once the credential is eleven
            months old

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
	f.StringVar(&opts.vcsURL, "vcs-url", os.Getenv(vcsURLEnv), "repository the forge credential is probed against (default: $"+vcsURLEnv+")")
	f.StringVar(&opts.identity, "identity", "", "address the mill commits under (default: mill@<host>)")
	f.StringVar(&opts.name, "name", defaultForgeUser, "name on the mill's commits")
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file the notify command and the workspace kind are read from when no flag names them")
	f.StringVar(&opts.notifyCommand, "notify-command", "", notifyCommandUsage)
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
	// a workspace no run works in.
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
	if config.SandboxRoot != "" && unset(cmd, "sandbox-root") {
		opts.sandboxRoot = config.SandboxRoot
	}
	applyGatewayConfig(cmd, config, &opts.gateway)
	opts.credentialMinted = config.ModelCredentialMinted
	probes := doctor(cmd.Context(), opts)
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

// doctor runs every probe in order and returns what each found. It is the
// value `mill mcp` renders too, so an agent asked to look at a sick host is
// told the same nine things.
func doctor(ctx context.Context, opts doctorOptions) []probe {
	socket, schema := probeService(ctx, opts)
	return []probe{
		socket,
		schema,
		probeDagger(ctx, opts),
		probeSidecar(ctx, opts),
		probeSigning(opts),
		probeForge(ctx, opts),
		probeNotify(ctx, opts),
		probeWorkspace(opts, machine.NewSystemd(workspace.Spawner{}, machine.Commands{})),
		probeGateway(ctx, opts, time.Now()),
	}
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
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
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
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
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
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	nodes, err := dagger.Dial(opts.daggerSocket, opts.project).Ready(ctx, daggerv1.AssigneeKind_ASSIGNEE_KIND_AGENT)
	if err != nil {
		return fail("dagger", "%s did not answer as this account: %v", opts.daggerSocket, err)
	}
	return pass("dagger", "%s answers; this account is a principal on %s, %d node(s) ready for an agent",
		opts.daggerSocket, opts.project, len(nodes))
}

// probeSidecar starts the harness sidecar and asks it for a no-op. Nothing
// short of running it proves it runs: the entrypoint may be missing, node may
// be absent, the bundle may be stale.
//
// It is spawned under the environment mode a run would spawn it with, and the
// line says which — a sidecar that starts under `inherit` and not under
// `strict` is a mode away from a run that cannot start a session, and the
// probe is where that difference is meant to show up.
func probeSidecar(ctx context.Context, opts doctorOptions) probe {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	mode, err := claude.ParseEnvMode(opts.envMode)
	if err != nil {
		return fail("sidecar", "%v", err)
	}
	adapter := claude.New(claude.WithEntrypoint(claude.DefaultCommand, opts.sidecar), claude.WithEnvMode(mode))
	terminal, err := adapter.Probe(ctx)
	if err != nil {
		return fail("sidecar", "%s did not answer under environment mode %s: %v", opts.sidecar, mode, err)
	}
	return pass("sidecar", "%s starts under environment mode %s and answers: %s",
		opts.sidecar, mode, firstLine(terminal.FinalMessage))
}

// probeSigning finds the mill's signing key and looks for its public key in
// an allowed_signers file of a project the mill has checked out. A key
// nothing trusts signs commits nobody can verify, which is the failure this
// probe exists to catch (docs/architecture.md §"Identity and credentials").
func probeSigning(opts doctorOptions) probe {
	dir := filepath.Join(opts.workspaceDir, "signing")
	key, err := signing.Load(dir, opts.name, opts.identity)
	if err != nil {
		return fail("signing", "no signing key under %s: %v", dir, err)
	}
	blob := keyBlob(key.PublicKey)
	trusted, err := trustedBy(opts.workspaceDir, blob)
	if err != nil {
		return fail("signing", "key %s: %v", opts.identity, err)
	}
	if len(trusted) == 0 {
		return fail("signing", "key %s is present but no project's allowed_signers names it", opts.identity)
	}
	return pass("signing", "key %s is present and trusted by %s", opts.identity, strings.Join(trusted, ", "))
}

// trustedBy is the projects whose checkout carries an allowed_signers file
// naming the key. Checkouts are the only copy of a project's repository this
// host has, so they are where the question is asked.
func trustedBy(workspaceDir, blob string) ([]string, error) {
	entries, err := os.ReadDir(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", workspaceDir, err)
	}
	var trusted []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "signing" {
			continue
		}
		for _, name := range []string{"allowed_signers", ".allowed_signers"} {
			path := filepath.Join(workspaceDir, entry.Name(), "checkout", name)
			raw, err := os.ReadFile(path) //nolint:gosec // a path under the worker's own workspace directory
			if err != nil {
				continue
			}
			if blob != "" && strings.Contains(string(raw), blob) {
				trusted = append(trusted, entry.Name())
				break
			}
		}
	}
	return trusted, nil
}

// keyBlob is the base64 body of an SSH public key: the part an
// allowed_signers line must carry, without the comment that follows it.
func keyBlob(publicKey string) string {
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// probeForge asks the forge about a branch that does not exist. The answer
// does not matter; being answered does. A credential the forge rejects fails
// here rather than four steps into a run.
func probeForge(ctx context.Context, opts doctorOptions) probe {
	if opts.vcsURL == "" {
		return fail("forge", "no repository to probe: pass --vcs-url or set %s", vcsURLEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	f, err := forges().For(
		forge.Project{Slug: opts.project, VCSURL: opts.vcsURL},
		forge.Static(envOr(forgeUserEnv, defaultForgeUser), os.Getenv(forgeTokenEnv)))
	if err != nil {
		return fail("forge", "%s: %v", opts.vcsURL, err)
	}
	// A branch nothing has ever pushed. "No pull request" is the forge
	// answering, which is what the credential is being probed for.
	_, err = f.PR(ctx, "mill-doctor-probe")
	if err == nil || errors.Is(err, forge.ErrNoPullRequest) {
		return pass("forge", "%s accepts the credential", opts.vcsURL)
	}
	return fail("forge", "%s refused the credential: %v", opts.vcsURL, err)
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
func probeWorkspace(opts doctorOptions, driver machine.Driver) probe {
	kind, err := workspace.ParseKind(opts.workspace)
	if err != nil {
		return fail("workspace", "%v", err)
	}
	if kind != workspace.KindMachine {
		return pass("workspace", "%s: a clone under the mill's account", kind)
	}
	fs, err := driver.Statfs(opts.sandboxRoot)
	if err != nil {
		return fail("workspace", "%s: sandbox root %s: %v", kind, opts.sandboxRoot, err)
	}
	if !fs.Btrfs() {
		return fail("workspace", "%s: sandbox root %s is %s, not %s (docs/sandbox.md §Storage)",
			kind, opts.sandboxRoot, fs.Type, machine.TypeBtrfs)
	}
	return pass("workspace", "%s: sandbox root %s is %s", kind, opts.sandboxRoot, fs.Type)
}

// firstLine is a message trimmed to one line, for a probe's reason.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if line == "" {
		return "(no message)"
	}
	return line
}
