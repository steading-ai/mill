package gateway_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/workspace"
)

// The live scenario: a real Claude Code session in a real machine with no
// login file, answering through this gateway on the host with the
// subscription credential the machine never holds. It is what
// docs/spikes/nspawn-per-run-sandbox.md §E14 proved with the spike's proxy,
// re-proved against the gateway a run uses.
//
// It runs on a host set up as docs/sandbox.md describes — root through
// sudo, the spike image, networkd addressing `ve-mill*` and the firewall
// admitting the gateway's port from it — and is off unless MILL_SANDBOX_HOST
// says so, because `make test` must need none of that.

const (
	liveVar         = "MILL_SANDBOX_HOST"
	subscriptionVar = "CLAUDE_CODE_OAUTH_TOKEN"
	image           = "/var/lib/machines/mill-image-spike"
	liveMachine     = "mill-gw57"
	liveToken       = "step-token-live-57-bearer-value"
)

// host runs argv on the host and returns what it said and its status.
func host(argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		Fail(fmt.Sprintf("%s: %v", strings.Join(argv, " "), err))
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

// inMachine is docs/sandbox.md §Exec: systemd-run -M as root with the
// properties named, the main process's status propagated.
func inMachine(props []string, argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	cmd := []string{"sudo", "systemd-run", "-M", liveMachine, "--pipe", "--wait", "--quiet", "--collect"}
	for _, p := range props {
		cmd = append(cmd, "-p", p)
	}
	return host(append(append(cmd, "--"), argv...)...)
}

// boot starts an ephemeral machine from the image with the host's CLI bound
// in, waits for it to run, and stops it with the spec. The veth is
// addressed by the host's networkd and the machine's own DHCP client, as a
// run's would be.
func boot(claude string) {
	GinkgoHelper()
	unit := "mill-run-" + liveMachine
	_, stderr, status := host("sudo", "systemd-run", "--unit="+unit, "--collect", "-p", "KillMode=mixed",
		"systemd-nspawn", "--machine="+liveMachine, "--boot", "--quiet", "--directory="+image, "--ephemeral",
		"--private-users=pick", "--private-users-ownership=auto", "--network-veth", "--settings=false",
		"--bind-ro="+claude+":/usr/local/bin/claude")
	Expect(status).To(BeZero(), stderr)
	DeferCleanup(func() { host("sudo", "systemctl", "stop", unit) })
	Eventually(func() int {
		_, _, status := inMachine(nil, "true")
		return status
	}).WithTimeout(30*time.Second).WithPolling(200*time.Millisecond).Should(BeZero(), "the machine's bus never answered")
	state, _, _ := inMachine(nil, "systemctl", "is-system-running", "--wait")
	Expect(strings.TrimSpace(state)).To(Equal("running"))
	Eventually(func() string {
		out, _, _ := inMachine(nil, "networkctl", "status", "host0", "--no-pager")
		return out
	}).WithTimeout(30*time.Second).WithPolling(500*time.Millisecond).Should(ContainSubstring("routable"), "host0 never got an address")
}

var _ = Describe("The credential gateway on a real machine", Label("live"), Serial, func() {
	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn, the spike image and the mill network to run the spec that boots a real machine")
		}
		if os.Getenv(subscriptionVar) == "" {
			Skip(subscriptionVar + " is not set; the gateway has no upstream credential to hold")
		}
		if _, _, status := host("sudo", "test", "-d", image); status != 0 {
			Skip(image + " is missing; spikes/nspawn/build-image.sh builds it")
		}
	})

	It("There is no login in the machine", func() {
		By("given a gateway on the host holding the subscription credential, from a file and not the environment")
		credentialFile := filepath.Join(GinkgoT().TempDir(), gateway.CredentialName)
		Expect(os.WriteFile(credentialFile, []byte(os.Getenv(subscriptionVar)+"\n"), 0o600)).To(Succeed())
		credential, err := gateway.LoadCredential(credentialFile)
		Expect(err).NotTo(HaveOccurred())
		var log bytes.Buffer
		g, err := gateway.New(gateway.Config{Credential: credential, Log: slog.New(slog.NewTextHandler(&log, nil))})
		Expect(err).NotTo(HaveOccurred())
		server, err := gateway.Listen(gateway.DefaultListen, g)
		if err != nil {
			Skip(fmt.Sprintf("the gateway's address is not this host's to bind: %v", err))
		}
		DeferCleanup(server.Close)
		token := workspace.TokenFor(liveToken)
		token.ID = "01STEPTOKENLIVE57"
		g.Rotate("01RUNLIVE57", gateway.Step{StepRunID: "01STEPRUNLIVE57", StepName: "implement", Token: token})

		By("and a machine with an empty ~/.claude")
		claude, err := exec.LookPath("claude")
		Expect(err).NotTo(HaveOccurred(), "the Claude Code CLI is not on the host's PATH")
		boot(claude)
		_, stderr, status := inMachine([]string{"User=agent"}, "sh", "-c", "test ! -e ~/.claude || rm -rf ~/.claude")
		Expect(status).To(BeZero(), stderr)

		By("when a session runs claude -p 'reply ok' with the step token and the gateway as its base URL")
		out, stderr, status := inMachine(
			[]string{"User=agent", "Environment=ANTHROPIC_BASE_URL=" + server.URL(), "Environment=ANTHROPIC_AUTH_TOKEN=" + liveToken},
			"claude", "-p", "reply with the single word ok", "--output-format", "json")
		Expect(status).To(BeZero(), stderr)

		By("then it answers")
		Expect(out).To(ContainSubstring(`"result":"ok"`))
		Expect(out).To(ContainSubstring(`"is_error":false`))

		By("and no .credentials.json exists in the machine afterwards")
		login, _, _ := inMachine([]string{"User=agent"}, "sh", "-c", "ls -a ~/.claude; test ! -e ~/.claude/.credentials.json && echo no-login-file")
		Expect(login).To(ContainSubstring("no-login-file"))

		By("and the gateway log shows the step token inbound and the subscription credential outbound, both by name and never by value")
		logged := log.String()
		_, _ = fmt.Fprintf(GinkgoWriter, "gateway log:\n%s", logged)
		Expect(logged).To(ContainSubstring("gateway: forwarding"))
		Expect(logged).To(ContainSubstring("token=01STEPTOKENLIVE57"))
		Expect(logged).To(ContainSubstring("upstream=api.anthropic.com"))
		Expect(logged).To(ContainSubstring("credential=" + credential.String()))
		Expect(logged).To(ContainSubstring("gateway: usage"))
		Expect(logged).NotTo(ContainSubstring(liveToken))
		Expect(logged).NotTo(ContainSubstring(os.Getenv(subscriptionVar)))
	})
})
