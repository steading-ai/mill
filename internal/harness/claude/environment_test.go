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
