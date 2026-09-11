package machine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// base is the pinned base digest every spec boots from.
const base = image.Digest("a3f1c2d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")

// The run as the ledger names it — a ULID, upper case — and as the host
// does.
const (
	runID = "01JRUNLIFECYCLE0000000000A"
	id    = "01jrunlifecycle0000000000a"
)

// fixture is a worker's worth of machine backend over the machined fake: a
// sandbox root, a machines directory, a run-state directory and a skill
// directory of the spec's own, the base subvolume in place, and a git
// stub recording what the clone carried.
type fixture struct {
	host     *machined
	root     string
	machines string
	runState string
	skill    string
	stubs    string
	events   string
	forge    *workerForge
	config   machine.Config
}

func newFixture(originURL string) *fixture {
	GinkgoHelper()
	tmp := GinkgoT().TempDir()
	f := &fixture{
		root:     filepath.Join(tmp, "mill"),
		machines: filepath.Join(tmp, "machines"),
		runState: filepath.Join(tmp, "run"),
		skill:    filepath.Join(tmp, "skill"),
		stubs:    filepath.Join(tmp, "stubs"),
		events:   filepath.Join(tmp, "stubs", "events"),
	}
	for _, dir := range []string{f.root, f.machines, f.runState, f.skill, f.stubs} {
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
	}
	Expect(os.Mkdir(filepath.Join(f.machines, image.Prefix+string(base)), 0o755)).To(Succeed())
	f.host = newMachined(f.events)
	f.forge = &workerForge{repo: forge.Repo{CloneURL: originURL}}
	f.config = machine.Config{
		Root:         f.root,
		Machines:     f.machines,
		RunState:     f.runState,
		Skill:        f.skill,
		Origin:       originURL,
		Forge:        f.forge,
		Git:          gitStub(f.stubs),
		Name:         "mill",
		Identity:     "mill@core",
		Images:       image.Config{Pin: string(base)},
		Limits:       machine.Limits{MemoryMax: "8G", CPUQuota: "400%", TasksMax: "4096"},
		ReadyTimeout: 5 * time.Second,
	}
	return f
}

func (f *fixture) workspace() *machine.Workspace { return machine.New(f.host, f.config) }

func (f *fixture) runDir() string    { return filepath.Join(f.root, "runs", id) }
func (f *fixture) socketDir() string { return filepath.Join(f.runState, id) }

func (f *fixture) manifest() machine.Manifest {
	GinkgoHelper()
	m, err := machine.ReadManifest(filepath.Join(f.runDir(), machine.ManifestName))
	Expect(err).NotTo(HaveOccurred())
	return m
}

// methods is the sequence of driver methods the fake recorded.
func (f *fixture) methods() []string {
	var out []string
	for _, call := range f.host.Calls() {
		out = append(out, call.Method)
	}
	return out
}

var _ = Describe("The machine workspace", Label("unit"), func() {
	var (
		f    *fixture
		spec workspace.Spec
	)

	BeforeEach(func() {
		f = newFixture(origin(nil))
		spec = workspace.Spec{RunID: runID, NodeRef: "mill#61", Origin: f.config.Origin}
	})

	// Scenario: A machine is a transient unit booting an ephemeral snapshot
	//   When a run creates its machine
	//   Then mill-machine@<run-id>.service is active
	//   And machinectl lists mill-<run-id> running
	//   And the machine's root is a snapshot that is gone after destroy
	It("A machine is a transient unit booting an ephemeral snapshot", func(ctx context.Context) {
		ws := f.workspace()

		By("when a run creates its machine")
		handle, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())

		By("then mill-machine@<run-id>.service is active")
		starts := f.host.CallsTo(driver.StartUnit)
		Expect(starts).To(HaveLen(1))
		Expect(starts[0].Args).To(Equal([]any{"mill-machine@" + id + ".service"}))
		Expect(handle.Sandbox.Unit).To(Equal("mill-machine@" + id + ".service"))

		By("and machinectl lists mill-<run-id> running")
		Expect(f.host.registered()).To(ConsistOf("mill-" + id))
		Expect(handle.Sandbox.Machine).To(Equal("mill-" + id))
		Expect(handle.Sandbox.Veth).To(HavePrefix("ve-mill-"))
		Expect(handle.Kind).To(Equal(workspace.KindMachine))
		Expect(handle.Root).To(Equal(filepath.Join(f.runDir(), "workspace")))
		Expect(handle.Socket).To(Equal(filepath.Join(f.socketDir(), "ws.sock")))
		Expect(handle.Branch).To(Equal("mill/mill-61"))

		By("and the machine's root is the ensured image, discarded with the machine after destroy")
		m := f.manifest()
		Expect(m.Image).To(Equal(string(base)), "no packages: the derived digest is the base's")
		Expect(handle.Sandbox.Image).To(Equal(string(base)))
		Expect(ws.Destroy(ctx)).To(Succeed())
		Expect(f.host.registered()).To(BeEmpty())
		Expect(f.runDir()).NotTo(BeADirectory())
		Expect(ws.Handle()).To(BeNil())
	})

	It("writes machine.json in the helper's schema, every bind under the run, the socket directory or the skill, and the ensured digest as the image", func(ctx context.Context) {
		f = newFixture(origin(map[string]string{
			"mise.toml": "[tools]\ngo = \"1.25\"\n",
			"mill.toml": "[cache.target]\npath = \"/work/target\"\n",
		}))
		spec.Origin = f.config.Origin
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		raw, err := os.ReadFile(filepath.Join(f.runDir(), machine.ManifestName))
		Expect(err).NotTo(HaveOccurred())
		By("the keys are exactly the helper's")
		Expect(string(raw)).To(ContainSubstring(`"image": "` + string(base) + `"`))
		Expect(string(raw)).To(ContainSubstring(`"limits": {`))
		Expect(string(raw)).To(ContainSubstring(`"MemoryMax": "8G"`))
		Expect(string(raw)).To(ContainSubstring(`"CPUQuota": "400%"`))
		Expect(string(raw)).To(ContainSubstring(`"TasksMax": "4096"`))
		Expect(string(raw)).To(ContainSubstring(`"nested": false`))
		for _, key := range []string{"source", "target", "readonly", "binds"} {
			Expect(string(raw)).To(ContainSubstring(`"` + key + `"`))
		}

		m := f.manifest()
		By("every bind's host side is under the run directory, the socket directory or the skill")
		allowed := func(source string) bool {
			for _, root := range []string{f.runDir(), f.socketDir(), f.skill} {
				if source == root || strings.HasPrefix(source, root+"/") {
					return true
				}
			}
			return false
		}
		targets := map[string]machine.Bind{}
		for _, bind := range m.Binds {
			Expect(allowed(bind.Source)).To(BeTrue(), "%s is outside every allowed root", bind.Source)
			targets[bind.Target] = bind
		}
		Expect(targets).To(HaveKey("/work"))
		Expect(targets["/work"].Source).To(Equal(filepath.Join(f.runDir(), "workspace")))
		Expect(targets["/run/mill"].Source).To(Equal(f.socketDir()))
		Expect(targets["/home/agent/.claude/skills/mill"]).To(Equal(machine.Bind{Source: f.skill, Target: "/home/agent/.claude/skills/mill", ReadOnly: true}))
		By("one bind per cache, at the lineage's path")
		Expect(targets["/home/agent/.cache/go-build"].Source).To(Equal(filepath.Join(f.runDir(), "cache", "go-build")))
		Expect(targets["/home/agent/go/pkg/mod"].Source).To(Equal(filepath.Join(f.runDir(), "cache", "go-mod")))
		Expect(targets["/home/agent/.local/share/mise"].Source).To(Equal(filepath.Join(f.runDir(), "cache", "mise")))
		Expect(targets["/work/target"].Source).To(Equal(filepath.Join(f.runDir(), "cache", "target")))
		for _, target := range []string{"/work", "/run/mill", "/work/target"} {
			Expect(targets[target].ReadOnly).To(BeFalse(), target)
		}
		By("and the proxy file is bound read-only where agent's user manager reads it at boot")
		proxy := targets["/etc/environment.d/50-mill-proxy.conf"]
		Expect(proxy.ReadOnly).To(BeTrue())
		Expect(proxy.Source).To(Equal(filepath.Join(f.runDir(), "environment.d", "50-mill-proxy.conf")))
		body, err := os.ReadFile(proxy.Source)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(ContainSubstring("HTTPS_PROXY=" + machine.DefaultProxy + "\n"))
		Expect(string(body)).To(ContainSubstring("HTTP_PROXY=" + machine.DefaultProxy + "\n"))
		Expect(string(body)).To(ContainSubstring("http_proxy=" + machine.DefaultProxy + "\n"))
		Expect(string(body)).To(ContainSubstring("NO_PROXY=" + machine.DefaultNoProxy + "\n"))
		Expect(string(body)).To(ContainSubstring("ANTHROPIC_BASE_URL=" + machine.DefaultGateway + "\n"))
		Expect(m.Check(map[string]bool{f.runDir(): false, f.socketDir(): false, f.skill: true})).To(Succeed())
	})

	// Scenario: Configuration is read from the checkout before the machine exists
	//   Given a repository whose mill.toml declares a package
	//   When the run is created
	//   Then the clone happened first, the derived image was ensured second, and the machine booted from it third
	It("Configuration is read from the checkout before the machine exists", func(ctx context.Context) {
		By("given a repository whose mill.toml declares a package")
		f = newFixture(origin(map[string]string{"mill.toml": "[image]\npackages = [\"libpq-devel\"]\n"}))
		spec.Origin = f.config.Origin
		ws := f.workspace()

		By("when the run is created")
		handle, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		By("then the clone happened first, the derived image was ensured second, and the machine booted from it third")
		Expect(lines(f.events)).To(Equal([]string{"clone", "ensure", "boot"}))
		derived := image.Derived(base, []string{"libpq-devel"})
		Expect(derived).NotTo(Equal(base))
		Expect(f.manifest().Image).To(Equal(string(derived)))
		Expect(handle.Sandbox.Image).To(Equal(string(derived)))
		Expect(filepath.Join(f.machines, image.Prefix+string(derived))).To(BeADirectory())
	})

	// Scenario: Readiness is systemd's
	//   When a run creates its machine
	//   Then no step exec is issued before is-system-running returns
	It("Readiness is systemd's", func(ctx context.Context) {
		ws := f.workspace()

		By("when a run creates its machine")
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		By("then the unit is started, machined is asked for running, and is-system-running --wait is exec'd, in that order")
		methods := f.methods()
		start := indexOf(methods, driver.StartUnit)
		shown := indexOf(methods, driver.ShowMachine)
		exec := indexOf(methods, driver.Exec)
		Expect(start).To(BeNumerically(">=", 0))
		Expect(shown).To(BeNumerically(">", start))
		Expect(exec).To(BeNumerically(">", shown))

		By("and no step exec is issued before is-system-running returns")
		execs := f.host.CallsTo(driver.Exec)
		Expect(execs).To(HaveLen(1), "the readiness probe is the only exec Create issues")
		request := execs[0].Args[0].(driver.ExecRequest)
		Expect(request.Argv).To(Equal([]string{"systemctl", "is-system-running", "--wait"}))
		Expect(request.Machine).To(Equal("mill-" + id))
		Expect(request.Slice).To(Equal(machine.ReadySlice))
	})

	It("A machine that never reaches ready is terminated, never stopped, and Create returns the timeout and the digest", func(ctx context.Context) {
		f.config.ReadyTimeout = 700 * time.Millisecond
		for range 100 {
			f.host.Script(driver.Exec, driver.Output{Stdout: "starting\n", Status: 1})
		}
		ws := f.workspace()

		_, err := ws.Create(ctx, spec)

		var timeout *machine.ReadyTimeoutError
		Expect(errors.As(err, &timeout)).To(BeTrue(), "%v", err)
		Expect(timeout.Timeout).To(Equal(700 * time.Millisecond))
		Expect(timeout.Image).To(Equal(string(base)))
		Expect(timeout.Machine).To(Equal("mill-" + id))
		Expect(err.Error()).To(ContainSubstring("700ms"))
		Expect(err.Error()).To(ContainSubstring(string(base)))
		By("the machine is gone and nothing of the run remains")
		Expect(f.host.CallsTo(driver.Terminate)).To(HaveLen(1))
		Expect(f.host.CallsTo(driver.StopUnit)).To(BeEmpty(), "a machine that never reported ready is never asked to stop")
		Expect(f.host.registered()).To(BeEmpty())
		Expect(f.runDir()).NotTo(BeADirectory())
		Expect(f.socketDir()).NotTo(BeADirectory())
		Expect(ws.Handle()).To(BeNil())
	})

	It("Destroy stops the unit with a bound, terminates if the machine is still registered, and refuses only if it still is", func(ctx context.Context) {
		f.host.lostStop = true
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		before := len(f.host.Calls())

		Expect(ws.Destroy(ctx)).To(Succeed())

		var order []string
		for _, call := range f.host.Calls()[before:] {
			if call.Method == driver.StopUnit || call.Method == driver.Terminate {
				order = append(order, call.Method)
			}
		}
		Expect(order).To(Equal([]string{driver.StopUnit, driver.Terminate}))
		Expect(f.host.CallsTo(driver.StopUnit)[0].Args).To(Equal([]any{"", "mill-machine@" + id + ".service"}))
		Expect(f.host.CallsTo(driver.Terminate)[0].Args).To(Equal([]any{"mill-" + id}))
		Expect(f.host.registered()).To(BeEmpty())
	})

	It("Destroy refuses to report finished while machined still lists the machine, naming it", func(ctx context.Context) {
		f.host.lostStop = true
		f.host.Script(driver.Terminate, errors.New("terminate: refused"))
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())

		err = ws.Destroy(ctx)

		Expect(err).To(MatchError(machine.ErrStillRegistered))
		Expect(err).To(MatchError(ContainSubstring("mill-" + id)))
		Expect(f.host.registered()).To(ConsistOf("mill-" + id))
		By("the run directory stays for the machine that is still there")
		Expect(f.runDir()).To(BeADirectory())
	})

	// Scenario: A run leaves nothing behind
	//   Given a run that wrote everywhere inside its machine
	//   When it is destroyed
	//   Then no machine, run directory, or unpromoted subvolume remains
	It("A run leaves nothing behind", func(ctx context.Context) {
		By("given a run with caches that wrote everywhere inside its machine")
		f = newFixture(origin(map[string]string{"mise.toml": "[tools]\ngo = \"1.25\"\n"}))
		spec.Origin = f.config.Origin
		ws := f.workspace()
		handle, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(handle.Sandbox.Lineages).To(ConsistOf("global/mise@0", "global/go-mod@0", "repo/"+cache.RepoID(f.config.Origin)+"/go-build@0"))
		copies, err := filepath.Glob(filepath.Join(f.runDir(), "cache", "*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(copies).To(HaveLen(3), "one writable copy per lineage")
		// What the machine wrote: into /work and into a cache, as agent
		// would through the binds; into its root, which is the ephemeral
		// snapshot nspawn discards and the fake never had.
		Expect(os.WriteFile(filepath.Join(handle.Root, "scratch.txt"), []byte("written\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(f.runDir(), "cache", "go-build", "object"), []byte("built\n"), 0o600)).To(Succeed())
		Expect(ws.Candidate(ctx, "test")).To(Succeed(), "the step closed on evidence")

		By("when it is destroyed")
		Expect(ws.Destroy(ctx)).To(Succeed())

		By("then no machine, run directory, or unpromoted subvolume remains")
		Expect(f.host.registered()).To(BeEmpty())
		Expect(f.runDir()).NotTo(BeADirectory())
		Expect(f.socketDir()).NotTo(BeADirectory())
		Expect(handle.Socket).NotTo(BeAnExistingFile())
		var subvolumes []string
		Expect(filepath.WalkDir(f.root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && strings.HasPrefix(entry.Name(), cache.GenerationPrefix) {
				subvolumes = append(subvolumes, strings.TrimPrefix(path, f.root))
			}
			return nil
		})).To(Succeed())
		Expect(subvolumes).To(ConsistOf(
			"/cache/global/mise/gen-1", "/cache/global/go-mod/gen-1", "/cache/repo/"+cache.RepoID(f.config.Origin)+"/go-build/gen-1",
		), "the promoted generations, and nothing else")
		Expect(filepath.Join(f.root, "cache", "repo", cache.RepoID(f.config.Origin), "go-build", "gen-1", "object")).To(BeAnExistingFile())
		Expect(f.host.CallsTo(driver.DeleteSubvolume)).NotTo(BeEmpty(), "the run's copies went through the driver")
	})

	It("The clone uses the worker's forge credential and no step token exists when it runs", func(ctx context.Context) {
		ws := f.workspace()

		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		By("the credential reached git through the environment, and git saw no step token")
		env := strings.Join(lines(filepath.Join(f.stubs, "git.env")), "\n")
		Expect(env).To(ContainSubstring("FORGE_TOKEN=" + workerToken))
		Expect(env).NotTo(ContainSubstring("STEP_TOKEN"))
		Expect(env).NotTo(ContainSubstring("ANTHROPIC_AUTH_TOKEN"))
		calls := lines(filepath.Join(f.stubs, "git.calls"))
		Expect(calls).To(ContainElement("clone"))
		Expect(calls).To(ContainElement(HavePrefix("credential.helper=")))
		By("the forge was bound to no step: Rotate had not been called and no token existed")
		Expect(f.forge.mints).To(Equal(1))
		Expect(f.forge.steps).To(Equal([]forge.Step{{}}))
		Expect(ws.Handle().Capabilities).To(BeEmpty())
		Expect(ws.Environment()[workspace.ModelGatewayTokenVar]).To(BeEmpty(), "no bearer until a step token is rotated in")
	})

	It("Attach reopens all of it for a resumed run", func(ctx context.Context) {
		first := f.workspace()
		created, err := first.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = first.Destroy(ctx) })

		By("a second worker on the host attaches by the run id alone")
		second := f.workspace()
		attached, err := second.Attach(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#61"})
		Expect(err).NotTo(HaveOccurred())
		Expect(attached.Sandbox).To(Equal(created.Sandbox))
		Expect(attached.Root).To(Equal(created.Root))
		Expect(attached.Branch).To(Equal(created.Branch))
		Expect(attached.Socket).To(Equal(created.Socket))
		Expect(attached.Socket).To(BeAnExistingFile())
		Expect(f.host.CallsTo(driver.StartUnit)).To(HaveLen(1), "nothing boots twice")
		Expect(second.Record().Ready()).To(BeTrue())

		By("and a run whose machine machined no longer knows is a lost sandbox")
		Expect(f.host.Terminate(ctx, "mill-"+id)).To(Succeed())
		_, err = f.workspace().Attach(ctx, workspace.Spec{RunID: runID})
		Expect(err).To(MatchError(machine.ErrLostSandbox))
		Expect(err).To(MatchError(ContainSubstring("mill-" + id)))
		Expect(f.host.CallsTo(driver.StartUnit)).To(HaveLen(1), "nothing resumes into a fresh machine")

		By("and a run this host never created has no record")
		_, err = f.workspace().Attach(ctx, workspace.Spec{RunID: "01JNEVERCREATED00000000000"})
		Expect(err).To(MatchError(machine.ErrNoRecord))
	})

	It("Rotate installs the token the gateway and the socket authenticate against, as the local backend does", func(ctx context.Context) {
		ws := f.workspace()
		handle, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		token := workspace.TokenFor("the-step-bearer", workspace.Read, workspace.Write)
		Expect(ws.Rotate(ctx, token)).To(Succeed())

		Expect(ws.Handle().Capabilities).To(Equal([]workspace.Capability{workspace.Read, workspace.Write}))
		env := ws.Environment()
		Expect(env[workspace.ModelGatewayTokenVar]).To(Equal("the-step-bearer"))
		Expect(env[workspace.ModelGatewayVar]).To(Equal(machine.DefaultGateway))
		Expect(env[workspace.HTTPSProxyVar]).To(Equal(machine.DefaultProxy))
		Expect(env[workspace.NoProxyVar]).To(Equal(machine.DefaultNoProxy))
		By("the zero token revokes")
		Expect(ws.Rotate(ctx, workspace.Token{})).To(Succeed())
		Expect(ws.Handle().Capabilities).To(BeEmpty())
		Expect(handle.Socket).To(BeAnExistingFile())
	})

	It("reads, writes and lists inside the checkout on the host and refuses paths outside it", func(ctx context.Context) {
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		content, err := ws.Read(ctx, "README.md")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal("as cloned\n"))
		Expect(ws.Write(ctx, "notes/plan.md", []byte("plan\n"))).To(Succeed())
		entries, err := ws.List(ctx, "notes")
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(Equal([]workspace.Entry{{Name: "plan.md", Size: 5}}))
		_, err = ws.Read(ctx, "../machine.json")
		Expect(err).To(MatchError(workspace.ErrOutsideTree))
		out, err := ws.GitRunner().Run(ctx, nil, "rev-parse", "--abbrev-ref", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("mill/mill-61"))
	})

	// Scenario: nested is a request the worker grants
	//   Given mill.toml requests nested and worker config does not grant it
	//   When the run is provisioned
	//   Then the machine is provisioned without nested
	//   And the first session receives a finding
	It("nested is a request the worker grants", func(ctx context.Context) {
		By("given mill.toml requests nested and worker config does not grant it")
		f = newFixture(origin(map[string]string{"mill.toml": "[machine]\nnested = true\n[cache.bad]\npath = \"/etc\"\n"}))
		spec.Origin = f.config.Origin
		f.config.NestedRepos = []string{"wellspring/other"}
		ws := f.workspace()

		By("when the run is provisioned")
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		By("then the machine is provisioned without nested")
		m := f.manifest()
		Expect(m.Nested).To(BeFalse())
		Expect(m.Binds).NotTo(ContainElement(HaveField("Target", "/var/lib/machines")))
		Expect(m.Binds).NotTo(ContainElement(HaveField("Target", machine.DaggerCheckoutTarget)))
		Expect(m.Image).To(Equal(string(base)), "the flat image, not the nested variant")

		By("and the first session receives a finding, beside mill.toml's other findings")
		var provisioned workspace.Provisioned = ws
		Expect(provisioned.Findings()).To(ContainElement(SatisfyAll(
			ContainSubstring("machine.nested"), ContainSubstring("not granted"), ContainSubstring("nested_repos"))))
		Expect(provisioned.Findings()).To(ContainElement(ContainSubstring("cache.bad.path")))
	})

	It("holds the shim's report of an install until the loop drains it, and refuses one naming nothing", func(ctx context.Context) {
		ws := f.workspace()
		Expect(ws.ReportInstall(ctx, []string{"vips-devel", " libpq-devel "})).To(Succeed())
		Expect(ws.ReportInstall(ctx, []string{"gcc"})).To(Succeed())
		Expect(ws.ReportInstall(ctx, []string{"", "  "})).To(MatchError(ContainSubstring("names no package")))

		Expect(ws.Installs()).To(Equal([]workspace.Install{
			{Packages: []string{"vips-devel", "libpq-devel"}},
			{Packages: []string{"gcc"}},
		}))
		Expect(ws.Installs()).To(BeEmpty(), "handed over once")
	})

	It("grants nested to a listed repository: machine.json gains nested, the two read-only binds and the nested image, and nothing else differs", func(ctx context.Context) {
		f = newFixture(origin(map[string]string{"mill.toml": "[machine]\nnested = true\n[image]\npackages = [\"libpq-devel\"]\n"}))
		spec.Origin = f.config.Origin
		f.config.DaggerCheckout = filepath.Join(f.root, "dagger")
		Expect(os.MkdirAll(f.config.DaggerCheckout, 0o755)).To(Succeed())
		provision := func(granted bool) map[string]any {
			GinkgoHelper()
			ws := f.workspace()
			_, err := ws.Create(ctx, spec)
			Expect(err).NotTo(HaveOccurred())
			raw, err := os.ReadFile(filepath.Join(f.runDir(), machine.ManifestName))
			Expect(err).NotTo(HaveOccurred())
			if granted {
				Expect(ws.Findings()).To(BeEmpty())
			} else {
				Expect(ws.Findings()).To(ConsistOf(ContainSubstring("machine.nested")))
			}
			Expect(ws.Destroy(ctx)).To(Succeed())
			var doc map[string]any
			Expect(json.Unmarshal(raw, &doc)).To(Succeed())
			return doc
		}

		By("provisioning the same run ungranted and then granted by its repository path")
		ungranted := provision(false)
		f.config.NestedRepos = []string{"/" + strings.TrimPrefix(f.config.Origin, "/")}
		granted := provision(true)

		By("then the diff of the two machine.json documents is nested, the two binds, and the nested variant of the image")
		Expect(ungranted["nested"]).To(BeFalse())
		Expect(granted["nested"]).To(BeTrue())
		flat := image.Derived(base, []string{"libpq-devel"})
		Expect(ungranted["image"]).To(Equal(string(flat)))
		Expect(granted["image"]).To(Equal(string(image.Derived(base, []string{"libpq-devel"}, image.Nested))),
			"the nested variant of the same packages: sudo for agent is baked, not bound")
		Expect(granted["limits"]).To(Equal(ungranted["limits"]))
		Expect(granted["binds"]).To(Equal(append(ungranted["binds"].([]any),
			map[string]any{"source": f.machines, "target": "/var/lib/machines", "readonly": true},
			map[string]any{"source": f.config.DaggerCheckout, "target": machine.DaggerCheckoutTarget, "readonly": true},
		)), "the ungranted binds in order, then the two read-only nested binds")
		for key := range granted {
			Expect([]string{"image", "binds", "limits", "nested"}).To(ContainElement(key), "no key beyond the helper's schema")
		}
		Expect(f.host.CallsTo(driver.BuildImageOnHost)).To(HaveLen(2), "one flat build, one nested build")
	})

	It("refuses a nested provision whose dagger checkout is not there, before the unit starts", func(ctx context.Context) {
		f = newFixture(origin(map[string]string{"mill.toml": "[machine]\nnested = true\n"}))
		spec.Origin = f.config.Origin
		f.config.NestedRepos = []string{f.config.Origin}
		f.config.DaggerCheckout = filepath.Join(f.root, "dagger")
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).To(MatchError(ContainSubstring("dagger")))
		Expect(f.host.CallsTo(driver.StartUnit)).To(BeEmpty())
	})

	It("answers the session's git and forge verbs with ErrNotBuilt naming the node that builds them", func(ctx context.Context) {
		ws := f.workspace()
		_, err := ws.Git().Status(ctx)
		Expect(err).To(MatchError(machine.ErrNotBuilt))
		Expect(err).To(MatchError(ContainSubstring(machine.VerbsNode)))
		_, err = ws.Forge().Push(ctx, workspace.PushRequest{})
		Expect(err).To(MatchError(machine.ErrNotBuilt))
	})

	It("refuses a second Create for a run directory that is already there", func(ctx context.Context) {
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		_, err = f.workspace().Create(ctx, spec)
		Expect(err).To(MatchError(machine.ErrExists))
		Expect(f.host.CallsTo(driver.StartUnit)).To(HaveLen(1))
	})
})

var _ = Describe("Names and the preflight", Label("unit"), func() {
	It("names the machine by the run id lowered, and refuses ids the helper would misread", func() {
		lowered, err := machine.IDFor(runID)
		Expect(err).NotTo(HaveOccurred())
		Expect(lowered).To(Equal(id))
		Expect(machine.MachineName(lowered)).To(Equal("mill-" + id))
		Expect(machine.UnitName(lowered)).To(Equal("mill-machine@" + id + ".service"))
		for _, verb := range []string{"exec", "stop", "terminate", "shutdown"} {
			_, err := machine.IDFor(verb)
			Expect(err).To(MatchError(machine.ErrRunID), verb)
		}
		_, err = machine.IDFor("run_1")
		Expect(err).To(MatchError(machine.ErrRunID))
		_, err = machine.IDFor("")
		Expect(err).To(MatchError(machine.ErrRunID))
	})

	It("checks a manifest the way the helper does, ahead of it", func() {
		allowed := map[string]bool{"/var/lib/mill/runs/x": false, "/run/mill/x": false, "/home/ivy/.local/share/mill/skill": true}
		good := machine.Manifest{Image: strings.Repeat("a", 64), Binds: []machine.Bind{
			{Source: "/var/lib/mill/runs/x/workspace", Target: "/work"},
			{Source: "/home/ivy/.local/share/mill/skill", Target: "/home/agent/.claude/skills/mill", ReadOnly: true},
		}}
		Expect(good.Check(allowed)).To(Succeed())

		bad := good
		bad.Binds = append(bad.Binds, machine.Bind{Source: "/home/ivy", Target: "/home/ivy"})
		Expect(bad.Check(allowed)).To(MatchError(machine.ErrManifest))
		Expect(bad.Check(allowed)).To(MatchError(ContainSubstring("binds[2].source")))
		bad = good
		bad.Binds[1].ReadOnly = false
		Expect(bad.Check(allowed)).To(MatchError(ContainSubstring("read-only")))
		bad = good
		bad.Image = "sha256:abc"
		Expect(bad.Check(allowed)).To(MatchError(ContainSubstring("image")))
		bad = good
		bad.Binds[0].Target = "/"
		Expect(bad.Check(allowed)).To(MatchError(ContainSubstring("target")))
		// The helper's own sysfs bind for a nested machine: neither its
		// target nor its source is a manifest's to name.
		reserved := machine.Manifest{Image: good.Image, Binds: []machine.Bind{{Source: "/var/lib/mill/runs/x/sys", Target: machine.HostSysfsTarget, ReadOnly: true}}}
		Expect(reserved.Check(allowed)).To(MatchError(ContainSubstring("binds[0].target /.hostsys is reserved")))
		sysfs := machine.Manifest{Image: good.Image, Binds: []machine.Bind{{Source: "/sys", Target: "/mnt/sys", ReadOnly: true}}}
		Expect(sysfs.Check(allowed)).To(MatchError(ContainSubstring("binds[0].source /sys is outside")))
	})

	It("Check refuses a sandbox root that is not btrfs, naming the filesystem, and a run-state directory the worker cannot write", func() {
		f := newFixture(origin(nil))
		f.host.Script(driver.Statfs, driver.Filesystem{Type: "xfs"})
		err := machine.Check(f.host, f.config)
		Expect(err).To(MatchError(ContainSubstring("xfs")))

		f.host.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs})
		Expect(machine.Check(f.host, f.config)).To(Succeed())

		f.host.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs})
		f.config.RunState = filepath.Join(f.runState, "absent")
		err = machine.Check(f.host, f.config)
		Expect(err).To(MatchError(ContainSubstring("absent")))
		Expect(err).To(MatchError(ContainSubstring("homestead")))
	})

	It("Check refuses a sandbox root outside the ephemeral root for a worker inside a machine, and lets a host alone", func() {
		f := newFixture(origin(nil))
		f.host.Script(driver.Container, driver.ContainerNspawn)
		f.config.Root = "/proc/mill"
		err := machine.Check(f.host, f.config)
		Expect(err).To(MatchError(driver.ErrOutsideEphemeralRoot))
		Expect(f.host.CallsTo(driver.Statfs)).To(BeEmpty(), "refused before anything else is probed")

		f.host.Script(driver.Container, "")
		f.host.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs})
		f.config.Root = f.root
		Expect(machine.Check(f.host, f.config)).To(Succeed(), "a host's root is wherever it is configured")
		Expect(machine.InsideMachine(context.Background(), f.host)).To(BeFalse())
		f.host.Script(driver.Container, driver.ContainerNspawn)
		Expect(machine.InsideMachine(context.Background(), f.host)).To(BeTrue())
	})

	It("takes the sandbox's default paths and network when none are configured", func() {
		Expect(machine.DefaultRoot).To(Equal("/var/lib/mill"))
		Expect(machine.DefaultMachines).To(Equal("/var/lib/machines"))
		Expect(machine.DefaultRunState).To(Equal("/run/mill"))
		Expect(machine.DefaultDaggerCheckout).To(Equal("/var/lib/mill/dagger"))
		Expect(machine.NestedRoot).To(Equal("/home/agent/mill"))
		Expect(machine.DefaultProxy).To(Equal("http://10.99.255.1:3128"))
		Expect(machine.DefaultGateway).To(Equal("http://10.99.255.1:8080"))
	})
})

// indexOf is the first index of want in list, or -1.
func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}
