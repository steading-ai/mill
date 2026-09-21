package mill_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// The package is what a host installs with no checkout and no build tools,
// so what it carries is the contract: the binary alone is a mill that
// starts, passes most probes, and fails at its first session. Both rpms are
// built once here and every spec reads them; bsdtar reads an rpm's payload
// on any host, so no rpm toolchain is needed to check one.
var _ = Describe("The package", Label("unit", "slow"), Ordered, func() {
	var rpms map[string]string // arch → rpm path

	BeforeAll(func() {
		dir := checkout()

		By("when make package runs")
		cmd := exec.Command("make", "package")
		cmd.Dir = dir
		cmd.Env = scratchEnv()
		session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Wait(10 * time.Minute)).To(gexec.Exit(0))

		By("then dist/ holds one rpm per architecture")
		rpms = map[string]string{}
		for arch, rpmArch := range map[string]string{"arm64": "aarch64", "amd64": "x86_64"} {
			matches, err := filepath.Glob(filepath.Join(dir, "dist", "mill-*."+rpmArch+".rpm"))
			Expect(err).NotTo(HaveOccurred())
			Expect(matches).To(HaveLen(1), "one %s rpm", rpmArch)
			rpms[arch] = matches[0]
		}
	})

	// list is every path the rpm installs.
	list := func(rpm string) []string {
		GinkgoHelper()
		out, err := exec.Command("bsdtar", "-tf", rpm).Output()
		Expect(err).NotTo(HaveOccurred())
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}

	// read is one installed file's content.
	read := func(rpm, path string) string {
		GinkgoHelper()
		out, err := exec.Command("bsdtar", "-xOf", rpm, path).Output()
		Expect(err).NotTo(HaveOccurred())
		return string(out)
	}

	// The packaged configuration's value for one key.
	setting := func(config, key string) string {
		GinkgoHelper()
		match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:\s*(\S+)`).FindStringSubmatch(config)
		Expect(match).NotTo(BeNil(), "the packaged configuration sets %s", key)
		return match[1]
	}

	It("carries the binary, the sidecar bundle, the workflows, the unit and its fragments", func() {
		for arch, rpm := range rpms {
			files := list(rpm)
			Expect(files).To(ContainElements(
				"/usr/bin/mill",
				"/usr/lib/mill/sidecar/dist/main.js",
				"/usr/lib/mill/sidecar/package.json",
				"/usr/lib/mill/sidecar/node_modules/@anthropic-ai/claude-agent-sdk/package.json",
				"/usr/share/mill/workflows/quick-fix/workflow.yaml",
				"/usr/share/mill/workflows/quick-fix/scripts/clean-tree",
				"/usr/share/mill/workflows/feature/workflow.yaml",
				"/usr/lib/systemd/system/mill.service",
				"/usr/lib/sysusers.d/mill.conf",
				"/usr/lib/tmpfiles.d/mill.conf",
				"/etc/mill/config.yaml",
				"/etc/mill/env",
			), "the %s package", arch)
		}
	})

	It("carries the SDK's native command for its own architecture, glibc only", func() {
		for arch, native := range map[string]string{"arm64": "linux-arm64", "amd64": "linux-x64"} {
			files := list(rpms[arch])
			Expect(files).To(ContainElement("/usr/lib/mill/sidecar/node_modules/@anthropic-ai/claude-agent-sdk-"+native+"/claude"), "the %s package", arch)
			for _, file := range files {
				Expect(file).NotTo(ContainSubstring("-musl/"), "the %s package carries no musl variant", arch)
				Expect(file).NotTo(MatchRegexp(`claude-agent-sdk-(darwin|win32)`), "the %s package carries no other platform", arch)
			}
		}
	})

	It("A packaged mill runs a session with the packaged sidecar", func() {
		for arch, rpm := range rpms {
			files := list(rpm)
			config := read(rpm, "/etc/mill/config.yaml")

			By("then the session starts from the installed sidecar bundle and not from any checkout")
			sidecar := setting(config, "sidecar")
			Expect(sidecar).To(HavePrefix("/usr/lib/mill/sidecar/"), "the %s package", arch)
			Expect(files).To(ContainElement(sidecar), "the %s package installs the entrypoint the configuration names", arch)
			workflowDir := setting(config, "workflow-dir")
			Expect(workflowDir).To(HavePrefix("/usr/share/mill/workflows/"), "the %s package", arch)
			Expect(files).To(ContainElement(workflowDir+"/workflow.yaml"), "the %s package installs the workflow the configuration names", arch)
			Expect(read(rpm, "/usr/lib/systemd/system/mill.service")).To(ContainSubstring("ExecStart=/usr/bin/mill serve --work"))
		}
	})

	It("The package does not pull the sandbox host half", func() {
		for arch, rpm := range rpms {
			By("given a machine with no polkit rules and no SELinux module for mill")
			for _, file := range list(rpm) {
				Expect(file).NotTo(MatchRegexp(`mill-machine|polkit|sudoers|selinux|\.pp$`), "the %s package carries none of the sandbox's host half", arch)
			}

			By("then it starts on the local backend and needs no host helper")
			config := read(rpm, "/etc/mill/config.yaml")
			Expect(setting(config, "workspace")).To(Equal("local"))
			for _, key := range []string{"image-registry", "proxy", "gateway-upstream", "sandbox-root", "machines-dir"} {
				Expect(config).NotTo(MatchRegexp(`(?m)^`+key+`:`), "the %s package names no sandbox host setting", arch)
			}
		}
	})

	It("keeps an edited configuration file across an upgrade", func() {
		// rpm keeps an edited file when the package marks it config and
		// noreplace, which nfpm records from the content's type. Whether a
		// real dnf upgrade honours the mark is the Fedora guest's to say;
		// what is asserted here is that the mark is asked for.
		body, err := os.ReadFile("packaging/nfpm.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(MatchRegexp(`dst: /etc/mill/config.yaml\n\s+type: config\|noreplace`))
		Expect(string(body)).To(MatchRegexp(`dst: /etc/mill/env\n\s+type: config\|noreplace`))
	})
})

// The unit the package installs runs under a dedicated account, and a
// deployment overrides that account with a drop-in and nothing else. What
// makes the override work is that the unit declares its directories rather
// than a scriptlet chowning them to `mill`.
var _ = Describe("The packaged unit", Label("unit"), func() {
	var unit string

	BeforeEach(func() {
		body, err := os.ReadFile("contrib/systemd/mill.service")
		Expect(err).NotTo(HaveOccurred())
		unit = string(body)
	})

	It("is a system unit under the mill account", func() {
		Expect(unit).To(MatchRegexp(`(?m)^User=mill$`))
		Expect(unit).To(MatchRegexp(`(?m)^WantedBy=multi-user.target$`))
		Expect(unit).To(MatchRegexp(`(?m)^ExecStart=/usr/bin/mill serve --work`))
	})

	It("declares its configuration, state and runtime directories so an overriding User= owns them", func() {
		Expect(unit).To(MatchRegexp(`(?m)^ConfigurationDirectory=mill$`))
		Expect(unit).To(MatchRegexp(`(?m)^StateDirectory=mill$`))
		Expect(unit).To(MatchRegexp(`(?m)^RuntimeDirectory=mill$`))
	})

	It("is not started, and nothing is chowned, by a scriptlet", func() {
		for _, script := range []string{"packaging/scripts/postinstall.sh", "packaging/scripts/preremove.sh", "packaging/scripts/postremove.sh"} {
			body, err := os.ReadFile(script)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).NotTo(MatchRegexp(`systemctl[^\n]*\bstart\b`), "%s starts nothing", script)
			Expect(string(body)).NotTo(ContainSubstring("chown"), "%s owns nothing", script)
			out, err := exec.Command("sh", "-n", script).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
		}
	})
})
