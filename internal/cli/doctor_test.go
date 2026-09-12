package cli_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("mill doctor", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	// probe is one line of doctor's report: the verdict, the probe, and the
	// reason.
	probe := func(output, name string) (verdict, reason string) {
		for _, line := range lines(output) {
			fields := strings.Fields(line)
			if len(fields) > 2 && fields[1] == name {
				return fields[0], strings.Join(fields[2:], " ")
			}
		}
		return "", ""
	}

	It("reports every probe as pass or fail with one line of reason", func() {
		m := serving(ctx)

		out, err := runCLI(ctx, m.socket, "doctor",
			// Nothing else on this host is configured, so every probe but
			// the two the service answers fails — and says why, which is
			// what the report is for.
			"--dagger-socket", filepath.Join(GinkgoT().TempDir(), "nothing.sock"),
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--config", filepath.Join(GinkgoT().TempDir(), "no-config.yaml"),
			"--project", "", "--vcs-url", "")
		Expect(err).To(HaveOccurred(), "a failed probe is a non-zero exit")

		// The table is every line but the refusal cli.Run adds after it.
		var reported []string
		for _, line := range lines(out) {
			if fields := strings.Fields(line); fields[0] == "pass" || fields[0] == "fail" || fields[0] == "warn" {
				reported = append(reported, fields[1])
			}
		}
		Expect(reported).To(Equal(cli.ProbeNames), "the probes reported are ProbeNames, in order")
		for _, name := range cli.ProbeNames {
			verdict, reason := probe(out, name)
			Expect(verdict).To(BeElementOf("pass", "fail"), "probe %s: %s", name, out)
			Expect(reason).NotTo(BeEmpty(), "probe %s carries no reason", name)
		}

		socket, reason := probe(out, "socket")
		Expect(socket).To(Equal("pass"))
		Expect(reason).To(ContainSubstring(m.socket))

		schema, reason := probe(out, "schema")
		Expect(schema).To(Equal("pass"))
		Expect(reason).To(ContainSubstring("current"))

		dagger, reason := probe(out, "dagger")
		Expect(dagger).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("no project to probe"))

		sidecar, reason := probe(out, "sidecar")
		Expect(sidecar).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("no-sidecar.js"))
		// The mode is on the line whether the probe passed or not: a
		// sidecar that starts under one mode and not the other is the
		// difference this probe exists to surface.
		Expect(reason).To(ContainSubstring("environment mode inherit"))

		signing, reason := probe(out, "signing")
		Expect(signing).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("no signing key"))

		forge, reason := probe(out, "forge")
		Expect(forge).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("no repository to probe"))
	})

	Describe("the notify probe", func() {
		// The probe runs the deployment's own notify command with a probe
		// notice, which is the only way to find out that a filed gate would
		// reach nobody before a gate is filed.
		notifyCommand := func(body string) string {
			GinkgoHelper()
			path := filepath.Join(GinkgoT().TempDir(), "notify.sh")
			Expect(os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700)).To(Succeed()) //nolint:gosec // a spec's own scratch script
			return path
		}

		doctorWith := func(command string) string {
			GinkgoHelper()
			m := serving(ctx)
			out, _ := runCLI(ctx, m.socket, "doctor",
				"--dagger-socket", filepath.Join(GinkgoT().TempDir(), "nothing.sock"),
				"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
				"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
				"--config", filepath.Join(GinkgoT().TempDir(), "no-config.yaml"),
				"--notify-command", command,
				"--project", "", "--vcs-url", "")
			return out
		}

		It("passes when the command accepts a probe notice", func() {
			out := doctorWith(notifyCommand(`grep -q '"kind":"probe"'`))

			verdict, reason := probe(out, "notify")
			Expect(verdict).To(Equal("pass"), out)
			Expect(reason).To(ContainSubstring("accepts a probe notice"))
		})

		It("fails when the command refuses it, and says what it said", func() {
			out := doctorWith(notifyCommand("echo 'no agent named ops' >&2; exit 3"))

			verdict, reason := probe(out, "notify")
			Expect(verdict).To(Equal("fail"), out)
			Expect(reason).To(ContainSubstring("exit 3"))
			Expect(reason).To(ContainSubstring("no agent named ops"))
		})

		It("passes and says so when no notify command is configured", func() {
			out := doctorWith("")

			verdict, reason := probe(out, "notify")
			Expect(verdict).To(Equal("pass"), out)
			Expect(reason).To(ContainSubstring("no notify command is configured"))
		})
	})

	It("reports the workspace kind in effect: local by default, machine when configured, with its sandbox root", func() {
		m := serving(ctx)

		out, _ := runCLI(ctx, m.socket, "doctor", "--project", "", "--vcs-url", "",
			"--config", filepath.Join(GinkgoT().TempDir(), "no-config.yaml"))
		verdict, reason := probe(out, "workspace")
		Expect(verdict).To(Equal("pass"))
		Expect(reason).To(HavePrefix("local:"))

		root := GinkgoT().TempDir()
		out, _ = runCLI(ctx, m.socket, "doctor", "--project", "", "--vcs-url", "",
			"--config", filepath.Join(GinkgoT().TempDir(), "no-config.yaml"),
			"--workspace", "machine", "--sandbox-root", root)
		_, reason = probe(out, "workspace")
		Expect(reason).To(HavePrefix("machine: sandbox root " + root + " is "))

		// The file is where a deployment says which backend it runs, so
		// the doctor reads it there too.
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte("workspace: machine\nsandbox-root: "+root+"\n"), 0o600)).To(Succeed())
		out, _ = runCLI(ctx, m.socket, "doctor", "--project", "", "--vcs-url", "", "--config", path)
		_, reason = probe(out, "workspace")
		Expect(reason).To(HavePrefix("machine: sandbox root " + root + " is "))
	})

	Describe("the gateway probe", func() {
		// One cheap request through the credential gateway a run would put
		// its sessions behind: a token count, which bills nothing. The
		// upstream is a stand-in here; on a deployment it is the API.
		doctorWith := func(args ...string) string {
			GinkgoHelper()
			m := serving(ctx)
			out, _ := runCLI(ctx, m.socket, append([]string{"doctor",
				"--dagger-socket", filepath.Join(GinkgoT().TempDir(), "nothing.sock"),
				"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
				"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
				"--project", "", "--vcs-url", ""}, args...)...)
			return out
		}
		credentialFile := func(value string) string {
			GinkgoHelper()
			path := filepath.Join(GinkgoT().TempDir(), "claude-oauth")
			Expect(os.WriteFile(path, []byte(value+"\n"), 0o600)).To(Succeed())
			return path
		}
		noConfig := func() string { return filepath.Join(GinkgoT().TempDir(), "no-config.yaml") }

		It("passes and says so when no gateway is configured", func() {
			out := doctorWith("--config", noConfig())
			verdict, reason := probe(out, "gateway")
			Expect(verdict).To(Equal("pass"))
			Expect(reason).To(ContainSubstring("no gateway is configured"))
		})

		It("fails the probe when the credential file is absent", func() {
			out := doctorWith("--config", noConfig(), "--gateway",
				"--model-credential-file", filepath.Join(GinkgoT().TempDir(), "claude-oauth"))
			verdict, reason := probe(out, "gateway")
			Expect(verdict).To(Equal("fail"))
			Expect(reason).To(ContainSubstring("no upstream credential"))
			Expect(reason).To(ContainSubstring("claude-oauth"))
		})

		It("passes when the upstream answers, and never prints the credential", func() {
			const credential = "sk-ant-oat01-doctor-spec-credential"
			var saw string
			upstream := countTokensUpstream(func(authorization string) { saw = authorization })
			out := doctorWith("--config", noConfig(), "--gateway",
				"--gateway-upstream", upstream, "--model-credential-file", credentialFile(credential))
			verdict, reason := probe(out, "gateway")
			// The upstream answered, so the probe passed; with no minted
			// date to age the credential from it says so, as a warning
			// rather than a failure.
			Expect(verdict).To(Equal("warn"), out)
			Expect(reason).To(ContainSubstring("answers a token count through the gateway"))
			Expect(reason).To(ContainSubstring("model_credential_minted is not set"))
			Expect(saw).To(Equal("Bearer " + credential))
			Expect(out).NotTo(ContainSubstring(credential))
		})

		It("fails when the upstream refuses the credential", func() {
			upstream := refusingUpstream()
			out := doctorWith("--config", noConfig(), "--gateway",
				"--gateway-upstream", upstream, "--model-credential-file", credentialFile("stale"))
			verdict, reason := probe(out, "gateway")
			Expect(verdict).To(Equal("fail"))
			Expect(reason).To(ContainSubstring("answered 401"))
		})

		It("warns at eleven months from model_credential_minted, read from the configuration file", func() {
			upstream := countTokensUpstream(func(string) {})
			config := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			minted := time.Now().AddDate(0, -11, -1).Format(time.DateOnly)
			Expect(os.WriteFile(config, []byte("gateway: true\ngateway-upstream: "+upstream+
				"\nmodel-credential-file: "+credentialFile("sk-ant-oat01-old")+
				"\nmodel_credential_minted: "+minted+"\n"), 0o600)).To(Succeed())

			out := doctorWith("--config", config)
			verdict, reason := probe(out, "gateway")
			Expect(verdict).To(Equal("warn"), out)
			Expect(reason).To(ContainSubstring("minted " + minted))
			Expect(reason).To(ContainSubstring("more than 11 months ago"))

			By("and does not warn on a credential minted this month")
			Expect(os.WriteFile(config, []byte("gateway: true\ngateway-upstream: "+upstream+
				"\nmodel-credential-file: "+credentialFile("sk-ant-oat01-fresh")+
				"\nmodel_credential_minted: "+time.Now().Format(time.DateOnly)+"\n"), 0o600)).To(Succeed())
			out = doctorWith("--config", config)
			verdict, _ = probe(out, "gateway")
			Expect(verdict).To(Equal("pass"), out)
		})
	})

	It("reports the environment mode a run would spawn the sidecar under", func() {
		m := serving(ctx)

		out, _ := runCLI(ctx, m.socket, "doctor",
			"--dagger-socket", filepath.Join(GinkgoT().TempDir(), "nothing.sock"),
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--env-mode", "strict",
			"--project", "", "--vcs-url", "")

		_, reason := probe(out, "sidecar")
		Expect(reason).To(ContainSubstring("environment mode strict"))
	})

	It("refuses an environment mode it has never heard of", func() {
		m := serving(ctx)

		out, err := runCLI(ctx, m.socket, "doctor",
			"--dagger-socket", filepath.Join(GinkgoT().TempDir(), "nothing.sock"),
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--env-mode", "relaxed",
			"--project", "", "--vcs-url", "")
		Expect(err).To(HaveOccurred())

		verdict, reason := probe(out, "sidecar")
		Expect(verdict).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("relaxed"))
	})

	It("says the socket did not answer when no service is listening", func() {
		out, err := runCLI(ctx, filepath.Join(GinkgoT().TempDir(), "absent.sock"), "doctor")
		Expect(err).To(HaveOccurred())

		socket, reason := probe(out, "socket")
		Expect(socket).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("did not answer"))

		schema, reason := probe(out, "schema")
		Expect(schema).To(Equal("fail"))
		Expect(reason).To(ContainSubstring("the service did not answer"))
	})

	It("reports dagger as reachable when it answers this account", func() {
		m := serving(ctx)
		g := daggerNode(ctx)

		out, _ := runCLI(ctx, m.socket, "doctor",
			"--dagger-socket", g.service.Socket, "--project", daggerProject,
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--vcs-url", "")

		verdict, reason := probe(out, "dagger")
		Expect(verdict).To(Equal("pass"), out)
		Expect(reason).To(ContainSubstring("this account is a principal on " + daggerProject))
	})
})

// countTokensUpstream is a stand-in for the API that answers a token count,
// reporting the bearer it was handed.
func countTokensUpstream(saw func(authorization string)) string {
	GinkgoHelper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":8}`))
	}))
	DeferCleanup(server.Close)
	return server.URL
}

// refusingUpstream is a stand-in that refuses every bearer.
func refusingUpstream() string {
	GinkgoHelper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid bearer token"}}`))
	}))
	DeferCleanup(server.Close)
	return server.URL
}
