package claude

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/workspace"
)

// The environment a sidecar is spawned with, asserted where it can be seen:
// the stub sidecar writes its own environment to a file, so what these specs
// read is the child's, after the workspace has assembled it, and not the map
// the adapter passed along.

var _ = Describe("The sidecar's environment", Label("unit"), func() {
	var (
		log      *recorder
		spawner  *watching
		session  harness.Session
		lifetime context.Context //nolint:containedctx // the spec's own deadline, held so every It shares it
	)

	BeforeEach(func() {
		log = &recorder{}
		spawner = &watching{Execer: workspace.Spawner{}}
		session = harness.Session{
			ID:        "b6f1e0a2-0000-4000-8000-000000000002",
			Cwd:       GinkgoT().TempDir(),
			Workspace: harness.Workspace{Socket: "/run/mill/ws.sock", Token: "step-token-1"},
			Timeout:   20 * time.Minute,
			Events:    log.sink(),
		}
		var cancel context.CancelFunc
		lifetime, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	// carrying puts a variable on the mill's own process, which is what both
	// modes are defined against.
	carrying := func(name, value string) {
		GinkgoHelper()

		Expect(os.Setenv(name, value)).To(Succeed())
		DeferCleanup(func() { Expect(os.Unsetenv(name)).To(Succeed()) })
	}

	// spawn runs a stub sidecar that writes its environment out, and returns
	// what the child saw along with the turn it produced. What is read back
	// is the child's own environment: /bin/sh adds PWD, SHLVL and _ of its
	// own on the way, which is why the closure assertion below is made
	// against the command the adapter handed the workspace.
	spawn := func(adapter *Adapter) (map[string]string, *harness.Turn) {
		GinkgoHelper()

		envFile := scratch("child.env")
		script := stub(`
env > ENVFILE
printf '%s\n' '{"terminal":{"exit":{"status":0}}}'
`, "ENVFILE", envFile)
		WithEntrypoint("/bin/sh", script)(adapter)

		turn, err := adapter.Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())
		return readEnvFile(envFile), turn
	}

	It("Strict mode is the floor and what the step asked for", func() {
		By("given env_mode strict and a mill process carrying DATABASE_URL, CLAUDECODE and GOFLAGS")
		carrying("DATABASE_URL", "postgres://mill@localhost/mill")
		carrying("CLAUDECODE", "1")
		carrying("GOFLAGS", "-mod=mod")

		By("and a step whose claude options name env.passthrough GOFLAGS")
		session.Options = map[string]any{"env": map[string]any{"passthrough": []any{"GOFLAGS"}}}

		By("when the adapter spawns the sidecar")
		env, turn := spawn(New(WithExecer(spawner), WithEnvMode(EnvStrict)))

		By("then the sidecar's environment holds HOME, PATH and GOFLAGS")
		Expect(env).To(HaveKey("HOME"))
		Expect(env).To(HaveKey("PATH"))
		Expect(env).To(HaveKeyWithValue("GOFLAGS", "-mod=mod"))

		By("and holds neither DATABASE_URL nor CLAUDECODE")
		Expect(env).NotTo(HaveKey("DATABASE_URL"))
		Expect(env).NotTo(HaveKey("CLAUDECODE"))

		By("and holds nothing beyond the floor, the socket, the token and what the step named")
		Expect(spawner.spawned()).To(HaveLen(1))
		spawned := spawner.spawned()[0]
		// ClearEnv is what makes the command's own map the whole of the
		// child's environment rather than an addition to the mill's.
		Expect(spawned.ClearEnv).To(BeTrue())
		allowed := append([]string{socketVar, tokenVar, "GOFLAGS"}, envFloor...)
		for name := range spawned.Env {
			Expect(allowed).To(ContainElement(name), "%s is outside the floor and the step named it nowhere", name)
		}
		Expect(env).To(HaveKeyWithValue(socketVar, "/run/mill/ws.sock"))
		Expect(env).To(HaveKeyWithValue(tokenVar, "step-token-1"))

		By("and the turn carries what the step asked for and what was stripped")
		Expect(turn.Environment.Passthrough).To(Equal([]string{"GOFLAGS"}))
		Expect(turn.Environment.Stripped).To(ContainElements("CLAUDECODE", "DATABASE_URL"))
	})

	It("Inherit mode keeps credentials out", func() {
		By("given env_mode inherit and a mill process carrying ANTHROPIC_API_KEY and GOFLAGS")
		carrying("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key")
		carrying("GOFLAGS", "-mod=mod")
		carrying("DATABASE_URL", "postgres://mill@localhost/mill")
		carrying("DG_PROJECT", "mill")
		carrying("CLAUDECODE", "1")

		By("when the adapter spawns the sidecar")
		env, turn := spawn(New(WithExecer(spawner)))

		By("then the sidecar's environment holds GOFLAGS, without the step asking for it")
		Expect(env).To(HaveKeyWithValue("GOFLAGS", "-mod=mod"))
		Expect(turn.Environment.Passthrough).To(BeEmpty())

		By("and not ANTHROPIC_API_KEY")
		Expect(env).NotTo(HaveKey("ANTHROPIC_API_KEY"))
		Expect(env).NotTo(HaveKey("DATABASE_URL"))
		Expect(env).NotTo(HaveKey("DG_PROJECT"))
		Expect(env).NotTo(HaveKey("CLAUDECODE"))

		By("and the step run records ANTHROPIC_API_KEY as stripped")
		// The ledger's half of this Then clause is the spec of the same name
		// in internal/run; what the adapter owns is reporting it.
		Expect(turn.Environment.Stripped).To(ContainElements(
			"ANTHROPIC_API_KEY", "CLAUDECODE", "DATABASE_URL", "DG_PROJECT"))
		Expect(sort.StringsAreSorted(turn.Environment.Stripped)).To(BeTrue())
	})

	It("A credential cannot be carved out", func() {
		By("given a step whose claude options name env.passthrough ANTHROPIC_API_KEY")
		carrying("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key")
		session.Options = map[string]any{"env": map[string]any{"passthrough": []any{"ANTHROPIC_API_KEY"}}}
		script := stub(`printf '%s\n' '{"terminal":{"exit":{"status":0}}}'`)

		By("when the run reaches the step, in either mode")
		for _, mode := range []EnvMode{EnvInherit, EnvStrict} {
			adapter := New(WithExecer(spawner), WithEnvMode(mode), WithEntrypoint("/bin/sh", script))
			_, err := adapter.Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)

			By("then the adapter refuses before spawning the sidecar")
			Expect(err).To(HaveOccurred(), "mode %s spawned a sidecar", mode)
			Expect(spawner.spawned()).To(BeEmpty())

			By("and the message names ANTHROPIC_API_KEY")
			Expect(err).To(MatchError(ContainSubstring("ANTHROPIC_API_KEY")))
		}
	})

	It("gives the sidecar the socket and the token whatever the mode", func() {
		By("given a mill process carrying a variable of its own")
		carrying("MILL_SPEC_LEAK", "must not reach the child")

		for _, mode := range []EnvMode{EnvInherit, EnvStrict} {
			env, _ := spawn(New(WithExecer(&watching{Execer: workspace.Spawner{}}), WithEnvMode(mode)))

			Expect(env).To(HaveKeyWithValue(socketVar, "/run/mill/ws.sock"), "mode %s", mode)
			Expect(env).To(HaveKeyWithValue(tokenVar, "step-token-1"), "mode %s", mode)
			Expect(env).NotTo(HaveKey("MILL_SPEC_LEAK"), "mode %s", mode)
		}
	})

	It("gives the sidecar the git identity the workspace decided, over the host's", func() {
		By("given a host account that commits as somebody else")
		carrying("GIT_AUTHOR_EMAIL", "somebody@elsewhere.test")
		carrying("GIT_CONFIG_GLOBAL", "/home/somebody/.gitconfig")

		By("and a workspace that has decided who the run commits as")
		session.Workspace.Env = map[string]string{
			"GIT_CONFIG_GLOBAL": "/run/mill/01J0RUN/gitconfig",
			"GIT_AUTHOR_EMAIL":  "mill@example.test",
		}

		for _, mode := range []EnvMode{EnvInherit, EnvStrict} {
			env, _ := spawn(New(WithExecer(&watching{Execer: workspace.Spawner{}}), WithEnvMode(mode)))

			By("then the sidecar runs under the workspace's, in " + string(mode) + " mode")
			Expect(env).To(HaveKeyWithValue("GIT_CONFIG_GLOBAL", "/run/mill/01J0RUN/gitconfig"), "mode %s", mode)
			Expect(env).To(HaveKeyWithValue("GIT_AUTHOR_EMAIL", "mill@example.test"), "mode %s", mode)
		}
	})

	// The workspace's way out: docs/sandbox.md §Network and §Credentials.
	// The five are the workspace's decisions and cross the floor the way its
	// git identity does; the host's own model credentials still stop at the
	// deny list, in either mode.
	decided := map[string]string{
		"HTTPS_PROXY":          "http://10.99.14.1:3128",
		"HTTP_PROXY":           "http://10.99.14.1:3128",
		"NO_PROXY":             "10.99.14.1",
		"ANTHROPIC_BASE_URL":   "http://10.99.14.1:8081",
		"ANTHROPIC_AUTH_TOKEN": "step-token-1",
	}

	It("The workspace's network decisions reach the session", func() {
		By("given --env-mode strict")
		adapter := New(WithExecer(spawner), WithEnvMode(EnvStrict))

		By("and a workspace that decided a proxy and a gateway")
		session.Workspace.Env = map[string]string{
			"GIT_CONFIG_GLOBAL": "/run/mill/01J0RUN/gitconfig",
			"GIT_AUTHOR_EMAIL":  "mill@example.test",
		}
		for name, value := range decided {
			session.Workspace.Env[name] = value
		}
		// The host's own proxy is not the workspace's, and the mill's
		// process carries things of its own besides.
		carrying("HTTPS_PROXY", "http://laptop-proxy.test:3128")
		carrying("GOFLAGS", "-mod=mod")

		By("when the adapter composes a sidecar's environment")
		env, _ := spawn(adapter)

		By("then HTTPS_PROXY, HTTP_PROXY, NO_PROXY, ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN are present")
		for name, value := range decided {
			Expect(env).To(HaveKeyWithValue(name, value))
		}

		By("and no variable outside the floor and the workspace's decisions is")
		Expect(spawner.spawned()).To(HaveLen(1))
		spawned := spawner.spawned()[0]
		Expect(spawned.ClearEnv).To(BeTrue())
		allowed := append([]string{socketVar, tokenVar}, envFloor...)
		for name := range session.Workspace.Env {
			allowed = append(allowed, name)
		}
		for name := range spawned.Env {
			Expect(allowed).To(ContainElement(name), "%s is outside the floor and the workspace decided it nowhere", name)
		}
		Expect(spawned.Env).NotTo(HaveKey("GOFLAGS"))
		Expect(env).To(HaveKeyWithValue("GIT_CONFIG_GLOBAL", "/run/mill/01J0RUN/gitconfig"))
	})

	It("The host's model credentials still do not", func() {
		By("given CLAUDE_CODE_OAUTH_TOKEN exported on the host")
		carrying("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-not-a-real-token")
		carrying("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key")
		session.Workspace.Env = map[string]string{}
		for name, value := range decided {
			session.Workspace.Env[name] = value
		}

		for _, mode := range []EnvMode{EnvInherit, EnvStrict} {
			By("when a sidecar is spawned in " + string(mode) + " mode")
			env, turn := spawn(New(WithExecer(&watching{Execer: workspace.Spawner{}}), WithEnvMode(mode)))

			By("then it is absent from the sidecar's environment")
			// The session's half of this Then clause is the spec of the
			// same name in sidecar/sidecar_test.go.
			Expect(env).NotTo(HaveKey("CLAUDE_CODE_OAUTH_TOKEN"), "mode %s", mode)
			Expect(env).NotTo(HaveKey("ANTHROPIC_API_KEY"), "mode %s", mode)
			Expect(turn.Environment.Stripped).To(ContainElements("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"))

			By("and the only ANTHROPIC_* the sidecar holds are the workspace's gateway and its bearer")
			for name := range env {
				if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_") {
					Expect(decided).To(HaveKey(name), "mode %s hands the session %s", mode, name)
				}
			}
			Expect(env).To(HaveKeyWithValue("ANTHROPIC_BASE_URL", "http://10.99.14.1:8081"), "mode %s", mode)
			Expect(env).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "step-token-1"), "mode %s", mode)
		}
	})

	It("The auth token follows the step", func() {
		adapter := New(WithExecer(spawner), WithEnvMode(EnvStrict))
		session.Workspace.Env = map[string]string{"ANTHROPIC_BASE_URL": "http://10.99.14.1:8081"}

		By("given step 1 ran with token A")
		session.Workspace.Token = "token-a"
		session.Workspace.Env["ANTHROPIC_AUTH_TOKEN"] = "token-a"
		first, _ := spawn(adapter)
		Expect(first).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "token-a"))

		By("when the workspace rotates to token B for step 2")
		// The run loop composes a session per step from the workspace's
		// environment after it rotates (internal/run/execute.go), so what
		// the adapter sees for step 2 is the rotated map.
		session.Workspace.Token = "token-b"
		session.Workspace.Env["ANTHROPIC_AUTH_TOKEN"] = "token-b"
		second, _ := spawn(adapter)

		By("then step 2's exec carries B")
		Expect(second).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "token-b"))
		Expect(second).To(HaveKeyWithValue(tokenVar, "token-b"))
		Expect(spawner.spawned()).To(HaveLen(2))
		Expect(spawner.spawned()[1].Env).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "token-b"))

		By("and no process still carries A")
		for name, value := range second {
			Expect(value).NotTo(Equal("token-a"), "%s still carries step 1's token", name)
		}
		// Step 1's sidecar — the one process that was ever given A — is gone
		// with its turn: the adapter spawns one sidecar per turn and waits
		// for it.
		Expect(gone(spawner.pids()[0])).To(BeTrue(), "step 1's sidecar is still running")
	})

	It("names the mode it was told about, and refuses one it has never heard of", func() {
		Expect(ParseEnvMode("inherit")).To(Equal(EnvInherit))
		Expect(ParseEnvMode("strict")).To(Equal(EnvStrict))
		_, err := ParseEnvMode("relaxed")
		Expect(err).To(MatchError(ContainSubstring("relaxed")))
	})
})

// readEnvFile parses what a stub sidecar's `env` wrote. A value spanning
// lines is not something these specs set, so a line with no `=` belongs to
// the value above it and is ignored.
func readEnvFile(path string) map[string]string {
	GinkgoHelper()

	raw, err := os.ReadFile(path) //nolint:gosec // a spec's own scratch file
	Expect(err).NotTo(HaveOccurred())
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env
}
