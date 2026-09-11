package nspawn_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The gateway of docs/sandbox.md §Credentials, as spikes/nspawn/gateway.py
// probes it: the write-up's E14.
const (
	gatewayScript = "gateway.py"
	// The subscription token the gateway holds; the live scenario needs a
	// real one and takes it from the environment, never from a file.
	subscriptionVar = "CLAUDE_CODE_OAUTH_TOKEN"
	stepToken       = "step-token-spec"
	// The veth between host and machine, addressed by hand: NetworkManager,
	// not networkd, runs the host, so nspawn's --network-veth brings no
	// address of its own (the write-up's host facts).
	hostAddr    = "10.99.14.1"
	machineAddr = "10.99.14.2"
	machine14   = "mill-e14"
)

// freePort asks the kernel for a port nothing listens on.
func freePort() int {
	GinkgoHelper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := l.Addr().(*net.TCPAddr).Port
	Expect(l.Close()).To(Succeed())
	return port
}

// gateway starts gateway.py with the environment given, bound to bind, its
// log in a file, and stops it with the spec. It returns the log's path.
func gateway(bind string, env ...string) (logPath string) {
	GinkgoHelper()
	logPath = filepath.Join(GinkgoT().TempDir(), "gateway.log")
	cmd := exec.Command("python3", gatewayScript)
	cmd.Env = append(append(os.Environ(), "GATEWAY_BIND="+bind, "GATEWAY_LOG="+logPath), env...)
	cmd.Stdout, cmd.Stderr = GinkgoWriter, GinkgoWriter
	Expect(cmd.Start()).To(Succeed())
	DeferCleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	Eventually(func() error {
		c, err := net.Dial("tcp", bind)
		if err == nil {
			_ = c.Close()
		}
		return err
	}).WithTimeout(10*time.Second).Should(Succeed(), "the gateway never listened on %s", bind)
	return logPath
}

var _ = Describe("The credential gateway", func() {
	It("Paths outside the Messages API are refused", func() {
		By("the gateway running")
		bind := fmt.Sprintf("127.0.0.1:%d", freePort())
		gateway(bind, subscriptionVar+"=not-a-real-token")

		By("a request is made to /v1/models")
		req, err := http.NewRequest(http.MethodGet, "http://"+bind+"/v1/models", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+stepToken)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		By("it returns 404")
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("The gateway pairs the step token with the upstream", func() {
		By("a request through the gateway")
		// A stand-in for api.anthropic.com that records the bearer it was
		// handed; the write-up's E14 is the same log against the real host.
		var upstreamSaw string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamSaw = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message"}`)
		}))
		DeferCleanup(upstream.Close)
		upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))

		const subscription = "subscription-token-spec"
		bind := fmt.Sprintf("127.0.0.1:%d", freePort())
		logPath := gateway(bind, subscriptionVar+"="+subscription, "GATEWAY_UPSTREAM="+upstream.URL)

		req, err := http.NewRequest(http.MethodPost, "http://"+bind+"/v1/messages", strings.NewReader(`{}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+stepToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(upstreamSaw).To(Equal("Bearer "+subscription), "the upstream must see the swapped bearer")

		By("the gateway logs it")
		var logged string
		Eventually(func() string {
			b, _ := os.ReadFile(logPath)
			logged = string(b)
			return logged
		}).WithTimeout(5 * time.Second).Should(ContainSubstring("response "))

		By("the log shows the step token inbound and the upstream host outbound, and never the subscription token")
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			if strings.HasPrefix(line, "request ") || strings.HasPrefix(line, "response ") {
				Expect(line).To(ContainSubstring("inbound-token=" + stepToken))
				Expect(line).To(ContainSubstring("outbound-host=" + upstreamHost))
			}
		}
		Expect(logged).To(ContainSubstring("inbound-token=" + stepToken))
		Expect(logged).NotTo(ContainSubstring(subscription))
	})

	Describe("on a real machine", Label("live"), Serial, func() {
		BeforeEach(func() {
			if os.Getenv(liveVar) == "" {
				Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine")
			}
			if os.Getenv(subscriptionVar) == "" {
				Skip(subscriptionVar + " is not set; the gateway has nothing to swap the step token for")
			}
			if _, _, status := run(nil, "sudo", "test", "-d", image); status != 0 {
				Skip(image + " is missing; spikes/nspawn/build-image.sh builds it")
			}
		})

		It("The session answers with no credential in the machine", func() {
			By("a machine whose agent has an empty ~/.claude")
			// The host's CLI, bound read-only, rather than an install through
			// a proxy the machine does not have.
			claude, err := exec.LookPath("claude")
			Expect(err).NotTo(HaveOccurred(), "the Claude Code CLI is not on the host's PATH")
			boot(machine14, "--bind-ro="+claude+":/usr/local/bin/claude")
			_, stderr, status := execIn(machine14, []string{"User=agent"}, "sh", "-c", "test ! -e ~/.claude || rm -rf ~/.claude")
			Expect(status).To(BeZero(), stderr)

			By("the veth addressed on both sides and admitted by the host's firewall")
			veth := "ve-" + machine14
			_, stderr, status = run(nil, "sudo", "sh", "-c", "ip addr add "+hostAddr+"/24 dev "+veth+" && ip link set "+veth+" up")
			Expect(status).To(BeZero(), stderr)
			if _, _, status := run(nil, "sudo", "firewall-cmd", "--state"); status == 0 {
				run(nil, "sudo", "firewall-cmd", "--zone=trusted", "--add-interface="+veth)
				DeferCleanup(func() { run(nil, "sudo", "firewall-cmd", "--zone=trusted", "--remove-interface="+veth) })
			}
			_, stderr, status = execIn(machine14, nil, "sh", "-c",
				"mkdir -p /run/systemd/network && printf '[Match]\\nName=host0\\n\\n[Network]\\nAddress="+machineAddr+"/24\\nLinkLocalAddressing=no\\n' > /run/systemd/network/10-host0.network && systemctl start systemd-networkd")
			Expect(status).To(BeZero(), stderr)

			By("a gateway on the host holding the subscription token")
			bind := fmt.Sprintf("%s:%d", hostAddr, freePort())
			logPath := gateway(bind)

			By("the session runs claude -p with the step token and the gateway as base URL")
			out, stderr, status := execIn(machine14,
				[]string{"User=agent", "Environment=ANTHROPIC_BASE_URL=http://" + bind, "Environment=ANTHROPIC_AUTH_TOKEN=" + stepToken},
				"claude", "-p", "reply with the single word ok", "--output-format", "json")
			Expect(status).To(BeZero(), stderr)

			By("the answer is ok and no .credentials.json exists in the machine")
			Expect(out).To(ContainSubstring(`"result":"ok"`))
			login, _, _ := execIn(machine14, []string{"User=agent"}, "sh", "-c", "ls -a ~/.claude; test ! -e ~/.claude/.credentials.json && echo no-login-file")
			Expect(login).To(ContainSubstring("no-login-file"))
			logged, _ := os.ReadFile(logPath)
			Expect(string(logged)).To(ContainSubstring("inbound-token=" + stepToken + " outbound-host=api.anthropic.com"))
			Expect(string(logged)).NotTo(ContainSubstring(os.Getenv(subscriptionVar)))
		})
	})
})
