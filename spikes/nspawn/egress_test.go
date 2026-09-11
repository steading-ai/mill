package nspawn_test

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The egress path of docs/sandbox.md §Network, the db suite of §Testing, the
// Harbor pulls of §Images and the shim of §The dnf shim, as the write-up's
// E5, E6, E11 and E12 ran them. Each scenario boots its own machine on the
// veth the gateway scenario uses, loads the table and starts squid on the
// host, and takes both down with the spec.
const (
	egressTable = "egress.nft"
	squidConf   = "squid.conf"
	shimScript  = "dnf"
	proxyPort   = 3128
	// The image Harbor is asked for, twice; docker.io resolves to the proxy
	// project through the image's registries.conf.d/harbor.conf.
	harborHost = "harbor.tailcbcc0e.ts.net"
	pullRef    = "docker.io/library/postgres:16"
	shimPkg    = "vips-devel"
)

func proxyURL() string { return fmt.Sprintf("http://%s:%d", hostAddr, proxyPort) }

// egress loads the mill table and starts squid on the host side of the veth.
// The address has to exist first — squid binds it and fails otherwise — so
// this runs after network. Both are removed with the spec; the package
// stays.
func egress() {
	GinkgoHelper()
	table, err := filepath.Abs(egressTable)
	Expect(err).NotTo(HaveOccurred())
	_, stderr, status := run(nil, "sudo", "nft", "-f", table)
	Expect(status).To(BeZero(), stderr)
	DeferCleanup(func() { run(nil, "sudo", "nft", "delete", "table", "inet", "mill") })

	conf, err := filepath.Abs(squidConf)
	Expect(err).NotTo(HaveOccurred())
	_, stderr, status = run(nil, "sudo", "sh", "-c", "cp "+conf+" /etc/squid/squid.conf && systemctl restart squid")
	Expect(status).To(BeZero(), stderr)
	DeferCleanup(func() { run(nil, "sudo", "systemctl", "stop", "squid") })
	Eventually(func() error {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", hostAddr, proxyPort), time.Second)
		if err != nil {
			return err
		}
		return conn.Close()
	}).WithTimeout(10*time.Second).Should(Succeed(), "squid never listened on the veth")
}

// network addresses both ends of the machine's veth, admits it to the host's
// firewall (the write-up's E5: core's default zone is drop, and inet mill's
// accept does not skip inet firewalld), and points the machine's resolver
// out through the forward the table permits.
func network(machine string) {
	GinkgoHelper()
	veth := "ve-" + machine
	_, stderr, status := run(nil, "sudo", "sh", "-c", "ip addr add "+hostAddr+"/24 dev "+veth+" && ip link set "+veth+" up")
	Expect(status).To(BeZero(), stderr)
	if _, _, status := run(nil, "sudo", "firewall-cmd", "--state"); status == 0 {
		run(nil, "sudo", "firewall-cmd", "--zone=trusted", "--add-interface=ve-mill+")
		DeferCleanup(func() { run(nil, "sudo", "firewall-cmd", "--zone=trusted", "--remove-interface=ve-mill+") })
	}
	_, stderr, status = execIn(machine, nil, "sh", "-c",
		"mkdir -p /run/systemd/network && printf '[Match]\\nName=host0\\n\\n[Network]\\nAddress="+machineAddr+"/24\\nGateway="+hostAddr+"\\nDNS=1.1.1.1\\nLinkLocalAddressing=no\\n' > /run/systemd/network/10-host0.network && systemctl start systemd-networkd")
	Expect(status).To(BeZero(), stderr)
	Eventually(func() int {
		_, _, status := execIn(machine, nil, "sh", "-c", "networkctl status host0 | grep -q 'State: routable'")
		return status
	}).WithTimeout(10*time.Second).Should(BeZero(), "host0 never became routable")
}

// proxyEnvironment writes the environment.d file the write-up's E6 found
// agent's user manager needs — podman's API service pulls with its own
// environment, not the exec's — and returns the bind that puts it in place.
func proxyEnvironment() string {
	GinkgoHelper()
	p := proxyURL()
	path := filepath.Join(GinkgoT().TempDir(), "50-mill-proxy.conf")
	body := fmt.Sprintf("HTTPS_PROXY=%s\nHTTP_PROXY=%s\nhttps_proxy=%s\nhttp_proxy=%s\nNO_PROXY=localhost,127.0.0.1\nno_proxy=localhost,127.0.0.1\n", p, p, p, p)
	Expect(os.WriteFile(path, []byte(body), 0o644)).To(Succeed())
	return "--bind-ro=" + path + ":/etc/environment.d/50-mill-proxy.conf"
}

// squidConnects returns the hosts squid tunnelled to since the log offset.
func squidConnects(since int) []string {
	GinkgoHelper()
	out, _, _ := run(nil, "sudo", "sh", "-c", fmt.Sprintf("tail -n +%d /var/log/squid/access.log | awk '$6 == \"CONNECT\" {print $7}'", since+1))
	return strings.Fields(out)
}

func squidLogLines() int {
	out, _, _ := run(nil, "sudo", "sh", "-c", "grep -c . /var/log/squid/access.log || true")
	n := 0
	_, _ = fmt.Sscan(strings.TrimSpace(out), &n) // an empty log reads as zero lines
	return n
}

var _ = Describe("Egress, the db suite, Harbor and the shim on a real machine", Label("live"), Serial, func() {
	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine")
		}
		if _, _, status := run(nil, "sudo", "test", "-d", image); status != 0 {
			Skip(image + " is missing; spikes/nspawn/build-image.sh builds it")
		}
		if _, _, status := run(nil, "sudo", "test", "-x", "/usr/sbin/squid"); status != 0 {
			Skip("squid is not installed on the host; docs/spikes/nspawn-per-run-sandbox.md §E5 installs it")
		}
	})

	It("Only the proxy reaches out", func() {
		By("a machine on a veth behind the mill nftables table")
		boot(machine14)
		network(machine14)
		egress()

		By("curl fetches https://example.com with and without HTTPS_PROXY")
		code, stderr, status := execIn(machine14, []string{"Environment=HTTPS_PROXY=" + proxyURL()},
			"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com/")
		Expect(status).To(BeZero(), stderr)
		cert, _, _ := execIn(machine14, []string{"Environment=HTTPS_PROXY=" + proxyURL()},
			"sh", "-c", `curl -sSv https://example.com/ 2>&1 >/dev/null | grep -E "issuer|subject"`)
		_, _, direct := execIn(machine14, nil, "curl", "-sS", "--max-time", "5", "-o", "/dev/null", "https://example.com/")

		By("the proxied fetch returns 200 with the origin's certificate and the direct one times out")
		Expect(code).To(Equal("200"))
		Expect(cert).To(ContainSubstring("subject: CN=example.com"))
		Expect(direct).To(Equal(28), "curl's timeout status")
	})

	It("The db suite runs inside a machine", func() {
		By("the mill checkout bound at /work and the machine's podman socket")
		checkout, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		dagger := os.Getenv("MILL_DAGGER_REPO")
		if dagger == "" {
			home, err := os.UserHomeDir()
			Expect(err).NotTo(HaveOccurred())
			dagger = filepath.Join(home, "src", "github.com", "ivy", "dagger")
		}
		if _, err := os.Stat(filepath.Join(dagger, "internal", "store", "migrations")); err != nil {
			Skip("no dagger checkout at " + dagger + "; the fixture needs its migrations and dg")
		}
		boot(machine14, "--bind="+checkout+":/work:idmap", "--bind-ro="+dagger+":/opt/dagger:idmap", proxyEnvironment())
		network(machine14)
		egress()
		socket := "/run/user/1000/podman/podman.sock"
		Eventually(func() int {
			_, _, status := execIn(machine14, []string{"User=agent"}, "test", "-S", socket)
			return status
		}).WithTimeout(30*time.Second).Should(BeZero(), "agent's podman socket never appeared")

		By("agent runs make test-db")
		env := []string{"User=agent", "WorkingDirectory=/work",
			"Environment=HTTPS_PROXY=" + proxyURL(), "Environment=HTTP_PROXY=" + proxyURL(),
			"Environment=DOCKER_HOST=unix://" + socket, "Environment=MILL_DAGGER_REPO=/opt/dagger"}
		_, stderr, status := execIn(machine14, env, "mise", "install")
		Expect(status).To(BeZero(), stderr)
		out, stderr, status := execIn(machine14, env, "mise", "exec", "--", "make", "test-db")

		By("the suite passes and no container remains")
		Expect(status).To(BeZero(), "make test-db inside: %s\n%s", stderr, out)
		Expect(out).To(ContainSubstring("Test Suite Passed"))
		ps := func() string {
			out, _, _ := execIn(machine14, []string{"User=agent"}, "podman", "ps", "-a", "--format", "{{.Names}}")
			return strings.TrimSpace(out)
		}
		// The fixture's containers are gone when the suite is; its reaper
		// keeps testsupport's five-minute reconnection window and then goes.
		for _, name := range strings.Fields(ps()) {
			Expect(name).To(HavePrefix("reaper_"), "a container other than ryuk survived the suite")
		}
		Eventually(ps).WithTimeout(6 * time.Minute).WithPolling(10 * time.Second).Should(BeEmpty())
	})

	It("Pulls resolve through Harbor", func() {
		By("registries.conf mapping docker.io to Harbor's proxy project")
		resp, err := http.Get("https://" + harborHost + "/api/v2.0/health")
		if err != nil || resp.StatusCode != http.StatusOK {
			Skip("Harbor is not healthy at " + harborHost)
		}
		Expect(resp.Body.Close()).To(Succeed())
		boot(machine14)
		network(machine14)
		egress()
		conf, _, _ := execIn(machine14, nil, "cat", "/etc/containers/registries.conf.d/harbor.conf")
		Expect(conf).To(ContainSubstring(`location = "` + harborHost + `/dockerhub-cache"`))

		By("agent pulls postgres:16 twice from fresh machines")
		since := squidLogLines()
		pull := func(machine string) string {
			GinkgoHelper()
			_, stderr, status := execIn(machine, []string{"User=agent", "Environment=HTTPS_PROXY=" + proxyURL()},
				"podman", "pull", "-q", pullRef)
			Expect(status).To(BeZero(), stderr)
			resolved, _, _ := execIn(machine, []string{"User=agent", "Environment=HTTPS_PROXY=" + proxyURL()}, "sh", "-c",
				"podman pull --log-level=debug "+pullRef+" 2>&1 | grep -o 'Trying to access [^ ]*'")
			return resolved
		}
		first := pull(machine14)
		run(nil, "sudo", "systemctl", "stop", "mill-run-"+machine14)
		boot("mill-e11")
		network("mill-e11")
		second := pull("mill-e11")

		By("the digest names Harbor and squid logged no upstream registry")
		// podman keeps the asked-for name in RepoDigests under a location
		// rewrite (the write-up's E11); the resolved reference is where the
		// digest came from.
		Expect(first).To(ContainSubstring(harborHost + "/dockerhub-cache/library/postgres:16"))
		Expect(second).To(ContainSubstring(harborHost + "/dockerhub-cache/library/postgres:16"))
		connects := squidConnects(since)
		Expect(connects).NotTo(BeEmpty())
		Expect(connects).To(HaveEach(HavePrefix(harborHost+":")), "squid tunnelled somewhere other than Harbor")
	})

	It("The shim hints and refuses removal", func() {
		By("the dnf shim ahead of /usr/bin/dnf and the sudoers line")
		boot(machine14)
		network(machine14)
		egress()
		shim, err := os.ReadFile(shimScript)
		Expect(err).NotTo(HaveOccurred())
		_, stderr, status := run(shim, "sudo", "systemd-run", "-M", machine14, "--pipe", "--wait", "--quiet", "--collect", "--",
			"sh", "-c", "cat > /usr/local/bin/dnf && chmod 0755 /usr/local/bin/dnf && grep -q 'NOPASSWD: /usr/bin/dnf install' /etc/sudoers.d/agent-dnf")
		Expect(status).To(BeZero(), stderr)

		By("agent installs vips-devel and then tries to remove it")
		out, stderr, status := execIn(machine14, []string{"User=agent", "Environment=HTTPS_PROXY=" + proxyURL()},
			"dnf", "install", "-y", shimPkg)
		Expect(status).To(BeZero(), stderr)
		_, removeErr, removeStatus := execIn(machine14, []string{"User=agent"}, "sudo", "-n", "/usr/bin/dnf", "remove", "-y", shimPkg)

		By("the install's last lines name [image].packages and the removal is refused")
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		Expect(len(lines)).To(BeNumerically(">=", 3))
		tail := strings.Join(lines[len(lines)-3:], "\n")
		Expect(tail).To(ContainSubstring("[image]"))
		Expect(tail).To(ContainSubstring(`packages = ["` + shimPkg + `"]`))
		Expect(removeStatus).NotTo(BeZero())
		Expect(removeErr).To(ContainSubstring("a password is required"))
	})
})
