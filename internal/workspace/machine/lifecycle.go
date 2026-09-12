package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/config"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
	"github.com/ivy/mill/internal/workspace/socket"
)

// Names inside a run's directory (docs/sandbox.md §Naming, §Filesystem).
const (
	// WorkspaceDir holds the checkout, bound at /work.
	WorkspaceDir = "workspace"
	// EnvironmentDir holds what is bound into the machine's
	// /etc/environment.d: the proxy file.
	EnvironmentDir = "environment.d"
	// ProxyFile carries the proxy and the gateway into agent's user
	// manager, and with it podman's API service, from boot
	// (docs/spikes/nspawn-per-run-sandbox.md E6: the exec's environment
	// does not reach the service that pulls).
	ProxyFile = "50-mill-proxy.conf"
	// ProxyTarget is where the machine sees it.
	ProxyTarget = "/etc/environment.d/" + ProxyFile
)

// ReadySlice is the step slice the readiness probe runs under: a step
// slice in the helper's pattern, `mill-step-<name>.slice`, so the driver's
// rule — no exec outside a slice — holds for the one exec that precedes
// every step, and the helper admits it.
const ReadySlice = "mill-step-ready.slice"

// OnlineArgv is the readiness probe's second exec: networkd's own wait for
// host0 to be routable over IPv4, bounded to a few seconds so the readiness
// loop, not wait-online, keeps the clock. Routable is the DHCP lease on the
// veth; the link-local address networkd assigns first satisfies `--ipv4`
// alone, and a machine with only that reaches neither the proxy nor the
// gateway.
var OnlineArgv = []string{
	"/usr/lib/systemd/systemd-networkd-wait-online",
	"--interface=host0", "--ipv4", "--operational-state=routable", "--timeout=5",
}

// Create is docs/sandbox.md §"Sequence of a run", steps 1 to 8: the run
// directory; the clone with the worker's forge credential; mill.toml read
// from the checkout; the base resolved and the derived image ensured; the
// caches restored; machine.json written; the unit started; readiness
// waited for; the socket started. Every step that fails after the run
// directory exists removes it, and a machine that does not reach ready is
// destroyed before the error is returned.
func (w *Workspace) Create(ctx context.Context, spec workspace.Spec) (*workspace.Handle, error) {
	if spec.RunID == "" {
		return nil, errors.New("machine: no run to create a workspace for")
	}
	id, err := IDFor(spec.RunID)
	if err != nil {
		return nil, err
	}
	origin := spec.Origin
	if origin == "" {
		origin = w.config.Origin
	}
	if origin == "" {
		return nil, errors.New("machine: no origin to clone")
	}
	if err := socket.CheckPath(filepath.Join(w.config.socketDir(id), SocketName)); err != nil {
		return nil, err
	}

	// Step 1: the run directory. Mkdir, not MkdirAll: the sandbox root is
	// the host's to provide, and a run name already taken is another
	// run's, or an earlier generation of this one.
	runDir := w.config.runDir(id)
	if err := os.Mkdir(w.config.runsDir(), 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("machine: create %s: %w", w.config.runsDir(), err)
	}
	if err := os.Mkdir(runDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrExists, runDir)
		}
		return nil, fmt.Errorf("machine: create %s: %w", runDir, err)
	}
	record := &Record{
		RunID: spec.RunID, ID: id,
		Machine: MachineName(id), Unit: UnitName(id),
		Origin: origin, Ref: spec.Ref,
		CreatedAt: w.config.Now(),
	}
	handle, err := w.create(ctx, spec, record)
	if err != nil {
		w.removeRunDirectory(context.WithoutCancel(ctx), id)
		return nil, err
	}
	return handle, nil
}

// create is Create once the run directory exists.
func (w *Workspace) create(ctx context.Context, spec workspace.Spec, record *Record) (*workspace.Handle, error) {
	id := record.ID
	runDir := w.config.runDir(id)
	checkout := filepath.Join(runDir, WorkspaceDir)

	// Step 2: the clone, with the worker's credential and no session in
	// sight.
	if err := w.clone(ctx, record.Origin, spec.Ref, checkout); err != nil {
		return nil, err
	}
	branch, err := w.checkoutBranch(ctx, checkout, spec)
	if err != nil {
		return nil, err
	}
	record.Branch = branch
	if record.Ref == "" {
		record.Ref = branch
	}

	// Step 3: mill.toml, from the checkout. Invalid is a finding.
	declared, findings, err := config.Load(checkout)
	if err != nil {
		return nil, err
	}
	for _, finding := range findings {
		record.Findings = append(record.Findings, finding.String())
	}
	nested := declared.Machine.Nested && w.nestedGranted(record.Origin)
	if declared.Machine.Nested && !nested {
		record.Findings = append(record.Findings,
			"mill.toml: machine.nested: requested but not granted by worker configuration (nested_repos does not list this repository); the machine is provisioned without nested")
	}
	record.Nested = nested

	// Steps 4 and 5: the base, then the derived image.
	base, err := w.images.ResolveBase(ctx)
	if err != nil {
		return nil, err
	}
	record.Base, record.BaseFallback = string(base.Digest), base.Fallback
	if base.Fallback {
		record.Findings = append(record.Findings, "base image: "+base.Reason)
	}
	var variants []image.Variant
	if nested {
		variants = append(variants, image.Nested)
	}
	digest, buildFinding, err := w.images.Ensure(ctx, base.Digest, declared.Image.Packages, variants...)
	if err != nil {
		return nil, err
	}
	if buildFinding != nil {
		record.Findings = append(record.Findings, buildFinding.String())
	}
	record.Image = string(digest)

	// Step 6: the caches.
	lineages, err := cache.Lineages(declared.Caches, cache.Repo(cache.RepoID(record.Origin)), checkout)
	if err != nil {
		return nil, err
	}
	restored, err := w.caches.Restore(ctx, id, lineages)
	if err != nil {
		return nil, err
	}
	record.Lineages = restored

	// Step 7: the machine. Every bind is in machine.json before the unit
	// starts; nothing can be added after.
	egress := spec.Egress
	if egress.Proxy == "" {
		egress.Proxy, egress.NoProxy = w.config.Proxy, w.config.NoProxy
	}
	if egress.Gateway == "" {
		egress.Gateway = w.config.Gateway
	}
	proxyFile, err := w.writeProxyFile(runDir, egress)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(w.config.socketDir(id), 0o700); err != nil {
		return nil, fmt.Errorf("machine: create %s: %w", w.config.socketDir(id), err)
	}
	if nested {
		// The helper refuses a bind whose source is not there, in the
		// unit's journal; the checkout is the worker's to see, so say it
		// here with the key that names it.
		if info, err := os.Stat(w.config.DaggerCheckout); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("machine: nested: dagger_checkout %s is not a directory to bind read-only at %s", w.config.DaggerCheckout, DaggerCheckoutTarget)
		}
	}
	manifest := w.manifest(declared, id, string(digest), proxyFile, nested)
	if err := manifest.Check(w.allowedBindRoots(id, nested)); err != nil {
		return nil, err
	}
	if err := manifest.Write(filepath.Join(runDir, ManifestName)); err != nil {
		return nil, err
	}
	if err := writeRecord(runDir, *record); err != nil {
		return nil, err
	}
	if err := w.host.StartUnit(ctx, record.Unit); err != nil {
		return nil, fmt.Errorf("machine: start %s: %w", record.Unit, err)
	}
	if err := w.images.Booted(digest); err != nil {
		// An image this worker has no record of — built by another
		// worker, or by hand — boots all the same; only eviction is
		// blind to it.
		w.logf("recording the boot of %s: %v", digest.Short(), err)
	}

	// Step 8: ready, or destroyed.
	if err := w.waitReady(ctx, record); err != nil {
		return nil, errors.Join(err, w.teardownUnready(context.WithoutCancel(ctx), record))
	}
	record.ReadyAt = w.config.Now()
	if names, err := w.host.MachineInterfaces(ctx, record.Machine); err == nil && len(names) > 0 {
		record.Veth = names[0]
	}
	if err := writeRecord(runDir, *record); err != nil {
		return nil, errors.Join(err, w.teardownReady(context.WithoutCancel(ctx), record))
	}
	handle, err := w.open(ctx, record, spec)
	if err != nil {
		return nil, errors.Join(err, w.teardownReady(context.WithoutCancel(ctx), record))
	}
	return handle, nil
}

// checkoutBranch puts the run's branch in place and returns it.
func (w *Workspace) checkoutBranch(ctx context.Context, checkout string, spec workspace.Spec) (string, error) {
	current, err := w.verbs.Host.BranchOf(ctx, checkout)
	if err != nil {
		return "", err
	}
	branch := workspace.BranchFor(spec.NodeRef)
	if branch != "" && current != branch {
		if _, err := w.verbs.Host.Run(ctx, checkout, nil, "checkout", "-B", branch); err != nil {
			return "", err
		}
		current = branch
	}
	return current, nil
}

// nestedGranted is whether worker configuration grants nested to an
// origin: `nested_repos` names it by its path on the forge or by its URL.
func (w *Workspace) nestedGranted(origin string) bool {
	path := repoPath(origin)
	for _, granted := range w.config.NestedRepos {
		if sameRepo(origin, granted) || (path != "" && strings.EqualFold(path, repoPath(granted))) {
			return true
		}
	}
	return false
}

// writeProxyFile writes the environment.d file the machine's user manager
// reads at boot, and returns its path. Both cases of the proxy names go
// in: curl reads the lower case ones only.
func (w *Workspace) writeProxyFile(runDir string, egress workspace.Egress) (string, error) {
	dir := filepath.Join(runDir, EnvironmentDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("machine: create %s: %w", dir, err)
	}
	var b strings.Builder
	b.WriteString("# Written by mill for one run: every connection out of this machine goes\n")
	b.WriteString("# through the host's proxy, and the model through its gateway.\n")
	for _, name := range []string{workspace.HTTPSProxyVar, workspace.HTTPProxyVar} {
		fmt.Fprintf(&b, "%s=%s\n%s=%s\n", name, egress.Proxy, strings.ToLower(name), egress.Proxy)
	}
	fmt.Fprintf(&b, "%s=%s\n%s=%s\n", workspace.NoProxyVar, egress.Bypass(), strings.ToLower(workspace.NoProxyVar), egress.Bypass())
	if egress.Gateway != "" {
		fmt.Fprintf(&b, "%s=%s\n", workspace.ModelGatewayVar, egress.Gateway)
	}
	path := filepath.Join(dir, ProxyFile)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil { //nolint:gosec // read by every user in the machine by design
		return "", fmt.Errorf("machine: write %s: %w", path, err)
	}
	return path, nil
}

// manifest is machine.json for a run: the configuration's binds — the
// workspace, the socket directory, the skill, the sidecar and the workflow
// directory read-only, one
// plus the proxy file read-only, and under nested the host's image
// subvolumes read-only so an inner suite boots them rather than building
// them, and the dagger checkout read-only for its test fixture. Nothing
// else differs between a nested and a flat provision.
func (w *Workspace) manifest(declared config.Config, id, digest, proxyFile string, nested bool) Manifest {
	mounts := config.Mounts{
		Run:     w.config.runDir(id),
		Socket:  w.config.socketDir(id),
		Skill:   w.config.Skill,
		Sidecar: w.config.Sidecar,
	}
	if w.config.WorkflowSource != "" {
		mounts.Workflow = w.config.Workflow
	}
	binds := config.Binds(declared, mounts)
	m := Manifest{Image: digest, Limits: w.config.Limits, Nested: nested}
	for _, bind := range binds {
		m.Binds = append(m.Binds, Bind{Source: bind.Host, Target: bind.Machine, ReadOnly: bind.ReadOnly})
	}
	m.Binds = append(m.Binds, Bind{Source: proxyFile, Target: ProxyTarget, ReadOnly: true})
	if nested {
		m.Binds = append(m.Binds,
			Bind{Source: w.config.Machines, Target: DefaultMachines, ReadOnly: true},
			Bind{Source: w.config.DaggerCheckout, Target: DaggerCheckoutTarget, ReadOnly: true})
	}
	return m
}

// allowedBindRoots is the helper's policy as this worker knows it: the run
// directory and the socket directory read-write, the skill, the sidecar and
// the workflow copy read-only, and under nested — only then — the image
// directory and the dagger checkout read-only.
func (w *Workspace) allowedBindRoots(id string, nested bool) map[string]bool {
	roots := map[string]bool{
		w.config.runDir(id):    false,
		w.config.socketDir(id): false,
		w.config.Skill:         true,
		w.config.Sidecar:       true,
	}
	if w.config.WorkflowSource != "" {
		roots[w.config.Workflow] = true
	}
	if nested {
		roots[w.config.Machines] = true
		roots[w.config.DaggerCheckout] = true
	}
	return roots
}

// waitReady is docs/sandbox.md §Provision's readiness: machined reports
// the machine running, then `systemctl is-system-running --wait` inside it
// returns — `running`, or `degraded` for a machine that booted with a
// failed unit, which is booted all the same — and then networkd reports
// host0 online with an IPv4 address, because systemd's state lands before
// the DHCP lease does and the first step's `mise install` would otherwise
// run with no route to the proxy. Under the configured timeout; over it,
// the error carries the digest and the last probe.
func (w *Workspace) waitReady(ctx context.Context, record *Record) error {
	deadline := time.NewTimer(w.config.ReadyTimeout)
	defer deadline.Stop()
	var last string
	for {
		ready, probe, err := w.probeReady(ctx, record)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if probe != "" {
			last = probe
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("machine: waiting for %s: %w", record.Machine, ctx.Err())
		case <-deadline.C:
			return &ReadyTimeoutError{Machine: record.Machine, Image: record.Image, Timeout: w.config.ReadyTimeout, Last: last}
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// probeReady is one readiness probe: not ready with what the probe said,
// or ready; an error only for a machine that has gone away.
func (w *Workspace) probeReady(ctx context.Context, record *Record) (ready bool, probe string, err error) {
	shown, err := w.host.ShowMachine(ctx, record.Machine)
	if err != nil {
		if errors.Is(err, driver.ErrNoMachine) {
			return false, "not registered with machined", nil
		}
		return false, "", err
	}
	if shown.State != driver.StateRunning {
		return false, "machinectl: State=" + shown.State, nil
	}
	process, err := w.host.Exec(ctx, driver.ExecRequest{
		Machine: record.Machine,
		Slice:   ReadySlice,
		Argv:    []string{"systemctl", "is-system-running", "--wait"},
	})
	if err != nil {
		return false, err.Error(), nil
	}
	_ = process.Stdin.Close()
	var stderr []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		stderr, _ = io.ReadAll(process.Stderr)
	}()
	stdout, _ := io.ReadAll(process.Stdout)
	<-done
	result := process.Wait()
	state := strings.TrimSpace(string(stdout))
	if result.Status == 0 || state == "degraded" {
		return w.probeOnline(ctx, record, state)
	}
	if result.Status == driver.HelperRefused {
		// The helper refused the exec itself, before the machine was
		// asked anything; no number of retries changes its answer.
		return false, "", fmt.Errorf("machine: readiness probe of %s refused by the helper: %s", record.Machine, strings.TrimSpace(string(stderr)))
	}
	return false, fmt.Sprintf("is-system-running exited %d: %s %s", result.Status, state, strings.TrimSpace(string(stderr))), nil
}

// probeOnline is the second half of a readiness probe, once systemd's state
// is in: `systemd-networkd-wait-online` for host0 with an IPv4 address,
// bounded so the readiness loop stays the loop. Ready when it exits zero;
// otherwise not yet, with systemd's state and wait-online's word as the
// probe.
func (w *Workspace) probeOnline(ctx context.Context, record *Record, state string) (ready bool, probe string, err error) {
	process, err := w.host.Exec(ctx, driver.ExecRequest{
		Machine: record.Machine,
		Slice:   ReadySlice,
		Argv:    OnlineArgv,
	})
	if err != nil {
		return false, err.Error(), nil
	}
	_ = process.Stdin.Close()
	_, _ = io.ReadAll(process.Stdout)
	stderr, _ := io.ReadAll(process.Stderr)
	result := process.Wait()
	if result.Status == 0 {
		return true, state, nil
	}
	if result.Status == driver.HelperRefused {
		return false, "", fmt.Errorf("machine: online probe of %s refused by the helper: %s", record.Machine, strings.TrimSpace(string(stderr)))
	}
	return false, fmt.Sprintf("%s; wait-online exited %d: %s", state, result.Status, strings.TrimSpace(string(stderr))), nil
}

// Destroy is docs/sandbox.md §Destroy: the socket closed, the machine
// stopped — with a bound, and only if it reported ready; terminated
// otherwise, or if it is still registered after the stop — the caches
// promoted from the last evidence-closed step or discarded, and the run
// directory and socket directory removed. A machine machined still lists
// after all that is [ErrStillRegistered], and the run is not finished.
func (w *Workspace) Destroy(ctx context.Context) error {
	closeErr := w.server.Close(ctx)
	w.mu.Lock()
	record := w.record
	w.record, w.handle, w.token = nil, nil, workspace.Token{}
	w.mu.Unlock()
	w.server.Rotate(workspace.Token{})
	if record == nil {
		return closeErr
	}
	var err error
	if record.Ready() {
		err = w.teardownReady(ctx, record)
	} else {
		err = w.teardownUnready(ctx, record)
	}
	if err != nil {
		// The machine is still there, so the run directory stays with it:
		// the reconciliation pass finds both by the run's name.
		w.mu.Lock()
		w.record = record
		w.mu.Unlock()
		return err
	}
	promoted, promoteErr := w.caches.Promote(ctx, record.ID)
	if promoteErr != nil && !errors.Is(promoteErr, os.ErrNotExist) {
		return promoteErr
	}
	for _, p := range promoted {
		w.logf("promoted %s to gen-%d (%d bytes)", p.Lineage, p.Generation, p.SizeBytes)
	}
	w.mu.Lock()
	w.promoted = record.lineages(promoted)
	w.mu.Unlock()
	w.removeRunDirectory(ctx, record.ID)
	if len(promoted) > 0 {
		// The trigger after a promotion is the cheap one: free space
		// against the watermarks, no expiry pass (docs/sandbox.md
		// §Eviction). What it cannot evict is a log line, never the run's
		// failure.
		report, err := w.caches.Evict(ctx, w.caches.Policy(w.images).WatermarkOnly())
		for _, d := range report.Deleted {
			w.logf("evicted %s %s: %s", d.Kind, d.Name(), d.Reason)
		}
		if err != nil {
			w.logf("eviction after promotion: %v", err)
		}
	}
	return closeErr
}

// Promoted is workspace.Promoter: the run's lineages as Destroy left them,
// each with the generation it promoted. Nil before Destroy has promoted.
func (w *Workspace) Promoted() []workspace.Lineage {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.promoted == nil {
		return nil
	}
	return append([]workspace.Lineage(nil), w.promoted...)
}

// teardownReady stops a machine that reported ready: `systemctl stop` of
// its unit under the stop bound, then the helper's terminate if machined
// still lists it, then a refusal if it still does.
func (w *Workspace) teardownReady(ctx context.Context, record *Record) error {
	stopCtx, cancel := context.WithTimeout(ctx, w.config.StopTimeout)
	defer cancel()
	stopErr := w.host.StopUnit(stopCtx, "", record.Unit)
	if w.gone(ctx, record.Machine, 2*time.Second) {
		return nil
	}
	if stopErr != nil {
		w.logf("stop %s: %v; terminating", record.Unit, stopErr)
	}
	return w.terminate(ctx, record)
}

// teardownUnready removes a machine that never reported ready. It is never
// asked to stop: a stop before the machine's init has its handlers is lost
// and hangs the unit for its whole stop timeout
// (docs/spikes/nspawn-per-run-sandbox.md E0). The helper's terminate ends
// it in about a second.
func (w *Workspace) teardownUnready(ctx context.Context, record *Record) error {
	if w.gone(ctx, record.Machine, 0) {
		return nil
	}
	return w.terminate(ctx, record)
}

// terminate is the helper's terminate, then the truth from machined.
func (w *Workspace) terminate(ctx context.Context, record *Record) error {
	if err := w.host.Terminate(ctx, record.Machine); err != nil {
		w.logf("terminate %s: %v", record.Machine, err)
	}
	if w.gone(ctx, record.Machine, 3*time.Second) {
		return nil
	}
	return fmt.Errorf("%w: %s (%s)", ErrStillRegistered, record.Machine, record.Unit)
}

// gone polls machined until it no longer knows the machine, for at most
// patience; true when it is gone.
func (w *Workspace) gone(ctx context.Context, name string, patience time.Duration) bool {
	deadline := time.Now().Add(patience)
	for {
		_, err := w.host.ShowMachine(ctx, name)
		if errors.Is(err, driver.ErrNoMachine) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// removeRunDirectory removes everything a run left on the host: its cache
// copies and candidates as subvolumes through the driver, the rest with
// the filesystem, and its socket directory.
func (w *Workspace) removeRunDirectory(ctx context.Context, id string) {
	if err := w.caches.Discard(ctx, id); err != nil {
		w.logf("discard caches of %s: %v", id, err)
	}
	if err := os.RemoveAll(w.config.runDir(id)); err != nil {
		w.logf("remove %s: %v", w.config.runDir(id), err)
	}
	if err := os.RemoveAll(w.config.socketDir(id)); err != nil {
		w.logf("remove %s: %v", w.config.socketDir(id), err)
	}
}

// logf writes one line to the configured log, through the redactor.
func (w *Workspace) logf(format string, args ...any) {
	if w.config.Log == nil {
		return
	}
	_, _ = fmt.Fprintln(w.config.Log, w.redactor.String("machine: "+fmt.Sprintf(format, args...)))
}
