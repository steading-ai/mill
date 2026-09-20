package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/forge/github"
	"github.com/ivy/mill/internal/testsupport"
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

	// forgeProbe is the forge line of doctor's report: the verdict and
	// the reason.
	forgeProbe := func(output string) (verdict, reason string) {
		for _, line := range lines(output) {
			fields := strings.Fields(line)
			if len(fields) > 2 && fields[1] == "forge" {
				return fields[0], strings.Join(fields[2:], " ")
			}
		}
		return "", ""
	}

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
