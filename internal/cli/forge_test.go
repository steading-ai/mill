package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/forge/github"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/signing"
)

// The GitHub forge as the mill reaches it: selected from the project's
// repository or from `forge:` in the configuration, probed by doctor with
// the credential a run would push with, and refused at startup when the
// configuration names a forge that does not exist.
var _ = Describe("The GitHub forge through the mill", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	const (
		scratchRepo = "https://github.com/octo-org/thing.git"
		repoAPI     = "/repos/octo-org/thing"
		mainHead    = "1111111111111111111111111111111111111111"
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		GinkgoT().Setenv("MILL_FORGE_TOKEN", "ghp_spec_token") // gitleaks:allow
	})

	// probeReason is one probe's line of doctor's report: the verdict and
	// the reason.
	probeReason := func(output, name string) (verdict, reason string) {
		for _, line := range lines(output) {
			fields := strings.Fields(line)
			if len(fields) > 2 && fields[1] == name {
				return fields[0], strings.Join(fields[2:], " ")
			}
		}
		return "", ""
	}
	// forgeProbe is the forge line of doctor's report.
	forgeProbe := func(output string) (verdict, reason string) { return probeReason(output, "forge") }

	// githubStub is a recorded api.github.com answering the reads doctor
	// makes, installed as the adapter's HTTP client for the spec.
	githubStub := func(answers map[string]githubAnswer) {
		GinkgoHelper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			answer, known := answers[r.Method+" "+r.URL.Path]
			if !known {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"the spec recorded no route for ` + r.Method + " " + r.URL.Path + `"}`))
				return
			}
			for name, value := range answer.headers {
				w.Header().Set(name, value)
			}
			w.WriteHeader(answer.status)
			_, _ = w.Write([]byte(answer.body))
		}))
		DeferCleanup(server.Close)
		target, err := url.Parse(server.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cli.SetForgeHTTPClient(&http.Client{Transport: toStub{target: target}}))
	}

	// everything is a token that can read all a delivery reads.
	everything := func() map[string]githubAnswer {
		return map[string]githubAnswer{
			"GET /user":                         {status: 200, body: `{"login":"octocat"}`},
			"GET " + repoAPI:                    {status: 200, body: `{"full_name":"octo-org/thing","default_branch":"main","allow_merge_commit":true}`},
			"GET " + repoAPI + "/branches/main": {status: 200, body: `{"name":"main","commit":{"sha":"` + mainHead + `"}}`},
			"GET " + repoAPI + "/commits/" + mainHead + "/check-runs": {status: 200, body: `{"total_count":0,"check_runs":[]}`},
			"GET " + repoAPI + "/commits/" + mainHead + "/status":     {status: 200, body: `{"state":"pending","total_count":0,"statuses":[]}`},
			"GET " + repoAPI + "/pulls":                               {status: 200, body: `[]`},
		}
	}

	doctorOn := func(g *gateFixture, extra ...string) string {
		GinkgoHelper()
		m := serving(ctx)
		args := append([]string{"doctor",
			"--dagger-socket", g.service.Socket, "--project", daggerProject,
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--config", filepath.Join(GinkgoT().TempDir(), "no-config.yaml"),
		}, extra...)
		out, _ := runCLI(ctx, m.socket, args...)
		return out
	}

	It("mill doctor names the account the token authenticates as", func() {
		githubStub(everything())
		g := daggerNodeWith(ctx, scratchRepo)

		verdict, reason := forgeProbe(doctorOn(g))
		Expect(verdict).To(Equal("pass"), reason)
		Expect(reason).To(ContainSubstring("as octocat"))
		Expect(reason).To(ContainSubstring("$MILL_FORGE_TOKEN"), "the credential is named either way")
	})

	It("mill doctor names the credential store when the token came from it", func() {
		// The token is delivered as $CREDENTIALS_DIRECTORY/forge-token, the
		// way ImportCredential=forge-token on the unit delivers it, and
		// MILL_FORGE_TOKEN is unset: the probe passes on the stored token
		// and says so.
		githubStub(everything())
		g := daggerNodeWith(ctx, scratchRepo)
		store := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(store, "forge-token"), []byte("ghp_from_the_store\n"), 0o600)).To(Succeed()) // gitleaks:allow
		GinkgoT().Setenv("CREDENTIALS_DIRECTORY", store)
		Expect(os.Unsetenv("MILL_FORGE_TOKEN")).To(Succeed())

		verdict, reason := forgeProbe(doctorOn(g))
		Expect(verdict).To(Equal("pass"), reason)
		Expect(reason).To(ContainSubstring("as octocat"))
		Expect(reason).To(ContainSubstring("credential store"))
		Expect(reason).To(ContainSubstring("$CREDENTIALS_DIRECTORY/forge-token"))
		Expect(reason).NotTo(ContainSubstring("$MILL_FORGE_TOKEN"))
	})

	It("An expired token is diagnosed before a run wastes work", func() {
		githubStub(map[string]githubAnswer{
			"GET /user": {status: 401, body: `{"message":"Bad credentials"}`},
		})
		g := daggerNodeWith(ctx, scratchRepo)

		verdict, reason := forgeProbe(doctorOn(g))
		Expect(verdict).To(Equal("fail"), reason)
		Expect(reason).To(ContainSubstring("$MILL_FORGE_TOKEN"), "the credential to replace is named")
		Expect(reason).To(ContainSubstring("expired or been revoked"))
	})

	It("mill doctor fails naming the permission a token lacks", func() {
		answers := everything()
		answers["GET "+repoAPI+"/pulls"] = githubAnswer{
			status: 403, body: `{"message":"Resource not accessible by personal access token"}`,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": "pull_requests=read"},
		}
		githubStub(answers)
		g := daggerNodeWith(ctx, scratchRepo)

		verdict, reason := forgeProbe(doctorOn(g))
		Expect(verdict).To(Equal("fail"), reason)
		Expect(reason).To(ContainSubstring("$MILL_FORGE_TOKEN"))
		Expect(reason).To(ContainSubstring("lacks Pull requests: read and write on octo-org/thing"))
	})

	It("selects the adapter the configuration names over the one the host suggests", func() {
		// A GitLab URL, delivered through the GitHub adapter because the
		// configuration says so: the probe reaches the stub, which is
		// only possible through that adapter.
		githubStub(everything())
		g := daggerNodeWith(ctx, "https://gitlab.example.test/octo-org/thing.git")

		verdict, reason := forgeProbe(doctorOn(g, "--forge", "github"))
		Expect(verdict).To(Equal("pass"), reason)
		Expect(reason).To(ContainSubstring("as octocat"))
	})

	It("mill doctor's sidecar, signing and forge probes are answered by the worker behind the socket, not by doctor's own local flags", func(ctx SpecContext) {
		// The worker's own signing key, generated ahead of a `serve --work`
		// that will hold it — no checkout ever exists for it to be found
		// in, so it can only pass through the repository fallback.
		workspaceDir := GinkgoT().TempDir()
		key, err := signing.Generate(ctx, workspace.Spawner{Floor: []string{"PATH=" + os.Getenv("PATH")}},
			filepath.Join(workspaceDir, "signing"), "mill", "mill@example.test")
		Expect(err).NotTo(HaveOccurred())

		answers := everything()
		answers["GET "+repoAPI+"/contents/allowed_signers"] = githubAnswer{
			status: 200,
			body:   `{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte(key.AllowedSignersLine()+"\n")) + `"}`,
		}
		githubStub(answers)
		g := daggerNodeWith(ctx, scratchRepo)

		databaseURL := pg.Database(ctx).Config().ConnString()
		socket := filepath.Join(GinkgoT().TempDir(), "mill.sock")
		config := filepath.Join(GinkgoT().TempDir(), "empty.yaml")
		Expect(os.WriteFile(config, []byte("{}\n"), 0o600)).To(Succeed())
		// Absent, so the reason it fails to start is the worker's own
		// sidecar path — the point of this spec is which path is named,
		// not whether a real sidecar answers.
		workerSidecar := filepath.Join(GinkgoT().TempDir(), "worker-sidecar.js")

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{
			"serve", "--work",
			"--socket", socket,
			"--database-url", databaseURL,
			"--config", config,
			"--project", daggerProject,
			// A label no node carries: what is under test is WorkerService
			// answering beside the worker, not what the worker claims.
			"--label", "nothing-carries-this",
			"--dagger-socket", g.service.Socket,
			"--sidecar", workerSidecar,
			"--identity", "mill@example.test",
			"--name", "mill",
			"--workspace-dir", workspaceDir,
			"--log-dir", filepath.Join(GinkgoT().TempDir(), "logs"),
		})

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			served <- root.ExecuteContext(serveCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(served, time.Minute).Should(Receive(BeNil()), stderr.String())
		})
		Eventually(func() error {
			_, err := os.Stat(socket)
			return err
		}, time.Minute, 50*time.Millisecond).Should(Succeed(), stderr.String())

		// doctor's own flags name none of the worker's settings — a
		// different project, a different sidecar, a different workspace —
		// so a worker-answered probe is distinguishable from one this host
		// would have produced on its own.
		out, _ := runCLI(ctx, socket, "doctor",
			"--dagger-socket", filepath.Join(GinkgoT().TempDir(), "nothing.sock"),
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "not-the-workers-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "not-the-workers-workspace"),
			"--config", filepath.Join(GinkgoT().TempDir(), "no-config.yaml"),
			"--project", "")

		verdict, reason := forgeProbe(out)
		Expect(verdict).To(Equal("pass"), reason)
		Expect(reason).To(ContainSubstring("as octocat"), "the worker's forge credential answered, not doctor's own unconfigured project")

		verdict, reason = probeReason(out, "signing")
		Expect(verdict).To(Equal("pass"), reason)
		Expect(reason).To(ContainSubstring("trusted by the project's repository"), "the worker's key, trusted by the repository fallback with no checkout")

		_, reason = probeReason(out, "sidecar")
		Expect(reason).To(ContainSubstring(workerSidecar), "the worker's own sidecar is the one probed, not doctor's --sidecar flag")
	})

	Describe("An unrecognised forge in configuration", func() {
		It("fails mill doctor's forge probe naming the value", func() {
			g := daggerNodeWith(ctx, scratchRepo)

			verdict, reason := forgeProbe(doctorOn(g, "--forge", "gitbucket"))
			Expect(verdict).To(Equal("fail"), reason)
			Expect(reason).To(ContainSubstring(`"gitbucket" is not a forge this mill has an adapter for: github, gitlab`))
		})

		It("stops the worker at startup naming the value", func() {
			g := daggerNodeWith(ctx, scratchRepo)
			databaseURL := pg.Database(ctx).Config().ConnString()
			config := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			Expect(os.WriteFile(config, []byte("forge: gitbucket\n"), 0o600)).To(Succeed())

			var stdout, stderr bytes.Buffer
			root := cli.NewRootCommand(&stdout, &stderr)
			root.SetArgs([]string{
				"serve", "--work",
				"--socket", filepath.Join(GinkgoT().TempDir(), "mill.sock"),
				"--database-url", databaseURL,
				"--config", config,
				"--project", daggerProject,
				"--dagger-socket", g.service.Socket,
				"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
				"--log-dir", filepath.Join(GinkgoT().TempDir(), "logs"),
			})

			err := root.ExecuteContext(ctx)

			Expect(err).To(MatchError(ContainSubstring(`"gitbucket" is not a forge this mill has an adapter for`)))
		})

		It("stops a run before anything is claimed, naming the value", func() {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", quickFix)
			g := daggerNodeWith(ctx, scratchRepo)
			var stdout, stderr bytes.Buffer

			exit := cli.Run([]string{
				"run", g.nodeRef,
				"--project", daggerProject, "--workflow", "quick-fix", "--forge", "gitbucket",
				"--socket", m.socket, "--dagger-socket", g.service.Socket,
				"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
				"--log-dir", filepath.Join(GinkgoT().TempDir(), "logs"),
			}, strings.NewReader(""), &stdout, &stderr)

			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).To(ContainSubstring(`"gitbucket" is not a forge this mill has an adapter for`))
		})
	})
})

// githubAnswer is one recorded answer from the stub GitHub.
type githubAnswer struct {
	status  int
	body    string
	headers map[string]string
}

// toStub sends every request the adapter addresses to api.github.com to the
// spec's stub instead.
type toStub struct{ target *url.URL }

func (t toStub) RoundTrip(r *http.Request) (*http.Response, error) {
	Expect(r.URL.String()).To(HavePrefix(github.API), "the adapter reaches one host")
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}
