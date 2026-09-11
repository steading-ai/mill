package nspawn_test

import (
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The nested machine of docs/sandbox.md §Self-hosting and the unprivileged
// boot of §Privilege, as the write-up's E13 and E7 ran them. The outer boots
// once for the three nested scenarios; the inner is an ephemeral machine the
// outer's agent starts through sudo, from a snapshot the outer takes of the
// host's read-only image bind. The host proxy is whatever the host runs —
// homestead's squid and `inet mill` on core — and the scenarios skip without
// it rather than replace it.
const (
	outer    = "mill-e13"
	inner    = "inner"
	writeUp  = "../../docs/spikes/nspawn-per-run-sandbox.md"
	outerUID = "1048576:65536"
	// outerFlags is the nested provision: a 65536-uid range (E13: identity
	// on the inner needs no more), the image subvolumes bound read-only and
	// idmapped, and the host's sysfs the inner's nspawn needs to see.
	outerFlags = "--boot --quiet --directory=" + image + " --ephemeral --private-users=" + outerUID +
		" --private-users-ownership=auto --network-veth --bind-ro=/var/lib/machines:/var/lib/machines:idmap --bind-ro=/sys:/.hostsys --settings=false"
	// innerTree is the outer's snapshot of the read-only bind: --ephemeral
	// snapshots beside its source, so the bind itself cannot be the source.
	innerTree = "/var/lib/mill-machines/mill-image-spike"
	// innerFlags is the one shape that boots one level down: identity
	// mapping (pick and any shift fail on the idmap or the chown), with a
	// fully visible proc and sysfs already in the tree when nspawn pivots.
	innerFlags = "--boot --quiet --directory=" + innerTree + " --ephemeral --private-users=identity --network-veth --settings=false" +
		" --bind-ro=/proc:/.hostproc --bind-ro=/.hostsys:/.hostsys"
	unprivileged = "mill-e7"
)

// agentIn runs argv as agent in the outer, through sudo when root is asked
// for: the nested variant gives agent passwordless sudo for everything.
func agentIn(root bool, argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	if root {
		argv = append([]string{"sudo"}, argv...)
	}
	return execIn(outer, []string{"User=agent"}, argv...)
}

// innerExec is `-M inner` from the outer, as agent through sudo.
func innerExec(props []string, argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	cmd := []string{"systemd-run", "-M", inner, "--pipe", "--wait", "--quiet", "--collect"}
	for _, p := range props {
		cmd = append(cmd, "-p", p)
	}
	return agentIn(true, append(append(cmd, "--"), argv...)...)
}

// outerGateway is the host side of the outer's veth, where the host proxy answers.
func outerGateway() string {
	GinkgoHelper()
	var gw string
	Eventually(func() string {
		out, _, _ := execIn(outer, nil, "networkctl", "status", "host0", "--no-pager")
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "Gateway:" {
				gw = f[1]
			}
		}
		return gw
	}).WithTimeout(15*time.Second).WithPolling(200*time.Millisecond).ShouldNot(BeEmpty(), "the outer's host0 never got a gateway")
	return gw
}

// bootOuter boots the outer with the nested provision, unmasks its /proc
// (E2), grants agent full sudo — the spike's stand-in for what the nested
// image variant bakes — installs nspawn and btrfs-progs through the host
// proxy, and snapshots the read-only image bind into the outer's own tree.
func bootOuter() {
	GinkgoHelper()
	unit := "mill-run-" + outer
	argv := append([]string{"sudo", "systemd-run", "--unit=" + unit, "--collect", "-p", "KillMode=mixed",
		"systemd-nspawn", "--machine=" + outer}, strings.Fields(outerFlags)...)
	_, stderr, status := run(nil, argv...)
	Expect(status).To(BeZero(), stderr)
	DeferCleanup(func() { run(nil, "sudo", "systemctl", "stop", unit) })

	Eventually(func() int {
		_, _, status := execIn(outer, nil, "true")
		return status
	}).WithTimeout(30*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(), "the outer's bus never answered")
	state, _, _ := execIn(outer, nil, "systemctl", "is-system-running", "--wait")
	Expect(strings.TrimSpace(state)).To(Equal("running"))

	for _, step := range [][]string{
		{"mount", "-t", "proc", "-o", "nosuid,nodev,noexec", "proc", "/proc"},
		{"sh", "-c", `echo "agent ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/agent-nested && chmod 0440 /etc/sudoers.d/agent-nested`},
		{"sh", "-c", "mkdir -p /run/systemd/nspawn/propagate && mount -t tmpfs tmpfs /run/systemd/nspawn/propagate"},
	} {
		_, stderr, status := execIn(outer, nil, step...)
		Expect(status).To(BeZero(), stderr)
	}
	proxy := "http://" + outerGateway() + ":3128"
	_, stderr, status = agentIn(true, "dnf", "install", "-y", "-q", "--setopt=proxy="+proxy, "systemd-container", "btrfs-progs")
	Expect(status).To(BeZero(), stderr)
	_, stderr, status = execIn(outer, nil, "sh", "-c", "mkdir -p "+innerTree+" && rmdir "+innerTree+" && btrfs subvolume snapshot "+image+" "+innerTree)
	Expect(status).To(BeZero(), stderr)
}

// bootInner has agent boot the inner through sudo and waits for its bus.
func bootInner() {
	GinkgoHelper()
	argv := append([]string{"systemd-run", "--unit=" + inner, "--collect", "systemd-nspawn", "--machine=" + inner}, strings.Fields(innerFlags)...)
	_, stderr, status := agentIn(true, argv...)
	Expect(status).To(BeZero(), stderr)
	Eventually(func() int {
		_, _, status := innerExec(nil, "true")
		return status
	}).WithTimeout(30*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(), "the inner's bus never answered")
	state, _, _ := innerExec(nil, "systemctl", "is-system-running", "--wait")
	Expect(strings.TrimSpace(state)).To(Equal("running"))
	_, stderr, status = innerExec(nil, "mount", "-t", "proc", "-o", "nosuid,nodev,noexec", "proc", "/proc")
	Expect(status).To(BeZero(), stderr)
	// The lease arrives after is-system-running; podman's first pull races it.
	Eventually(func() int {
		_, _, status := innerExec(nil, "sh", "-c", "networkctl status host0 --no-pager | grep -q 'Address: 192\\.'")
		return status
	}).WithTimeout(15*time.Second).Should(BeZero(), "the inner never got a lease from the outer")
}

func hostProxy() {
	GinkgoHelper()
	if _, _, status := run(nil, "systemctl", "is-active", "--quiet", "squid"); status != 0 {
		Skip("squid is not running on the host; docs/spikes/nspawn-per-run-sandbox.md §E13 uses homestead's")
	}
	if _, _, status := run(nil, "sudo", "nft", "list", "table", "inet", "mill"); status != 0 {
		Skip("the inet mill table is not loaded on the host")
	}
}

var _ = Describe("A machine inside a machine", Label("live"), Serial, func() {
	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine")
		}
		if _, _, status := run(nil, "sudo", "test", "-d", image); status != 0 {
			Skip(image + " is missing; spikes/nspawn/build-image.sh builds it")
		}
	})

	Describe("the nested outer", Ordered, func() {
		BeforeAll(func() {
			hostProxy()
			out, _, _ := run(nil, "sh", "-c", "findmnt -no OPTIONS /var/lib/machines | tr , '\\n' | grep -x user_subvol_rm_allowed")
			Expect(strings.TrimSpace(out)).To(Equal("user_subvol_rm_allowed"), "the host mount lacks user_subvol_rm_allowed")
			bootOuter()
			bootInner()
		})

		It("An inner machine boots inside the outer", func() {
			By("the inner registers with the outer's machined")
			list, stderr, status := agentIn(true, "machinectl", "list", "--no-pager", "--no-legend")
			Expect(status).To(BeZero(), stderr)
			Expect(list).To(MatchRegexp(`(?m)^inner\s+container\s+systemd-nspawn`), list)

			By("hello prints from podman inside the inner")
			proxy := "http://" + outerGateway() + ":3128"
			out, stderr, status := innerExec([]string{"User=agent", "Environment=HTTPS_PROXY=" + proxy, "Environment=https_proxy=" + proxy},
				"podman", "run", "--rm", hello)
			Expect(status).To(BeZero(), stderr)
			Expect(out).To(ContainSubstring("Hello Podman World"))
		})

		It("The outer can manage subvolumes as agent", func() {
			// E9's rule holds inside too: a read-only snapshot is made writable
			// before its owner deletes it.
			out, stderr, status := agentIn(false, "sh", "-c",
				"cd /home/agent && btrfs subvolume create s && btrfs subvolume snapshot -r s s-ro && btrfs property set -ts s-ro ro false && btrfs subvolume delete s s-ro && echo ok")
			Expect(status).To(BeZero(), stderr)
			Expect(strings.TrimSpace(out)).To(HaveSuffix("ok"))
		})

		It("The inner reaches the host proxy", func() {
			proxy := "http://" + outerGateway() + ":3128"
			out, stderr, status := innerExec([]string{"User=agent"},
				"curl", "-sS", "-m", "10", "-o", "/dev/null", "-w", "%{http_code}", "-x", proxy, "https://example.com")
			Expect(status).To(BeZero(), stderr)
			Expect(strings.TrimSpace(out)).To(Equal("200"))
		})
	})

	It("An unprivileged boot is measured, not assumed", func() {
		By("nsresourced and mountfsd enabled")
		_, stderr, status := run(nil, "sudo", "systemctl", "enable", "--now", "systemd-nsresourced.socket", "systemd-mountfsd.socket")
		Expect(status).To(BeZero(), stderr)
		DeferCleanup(func() {
			run(nil, "sudo", "systemctl", "disable", "--now", "systemd-nsresourced.socket", "systemd-mountfsd.socket")
		})
		// The source is root's and 0700; the copy the operator boots is made
		// with sudo, and readable, as the write-up's E7 records.
		copyPath := "/var/lib/mill/mill-image-" + unprivileged
		run(nil, "sudo", "btrfs", "subvolume", "delete", copyPath)
		_, stderr, status = run(nil, "sudo", "btrfs", "subvolume", "snapshot", image, copyPath)
		Expect(status).To(BeZero(), stderr)
		DeferCleanup(func() { run(nil, "sudo", "btrfs", "subvolume", "delete", copyPath) })

		By("the operator boots a machine without sudo, bounded")
		unit := "mill-run-" + unprivileged
		run(nil, "systemctl", "--user", "reset-failed", unit)
		_, stderr, status = run(nil, "systemd-run", "--user", "--unit="+unit, "--collect",
			"systemd-nspawn", "--boot", "--quiet", "--machine="+unprivileged, "--directory="+copyPath,
			"--ephemeral", "--private-users=managed", "--network-veth", "--settings=false")
		Expect(status).To(BeZero(), stderr)
		DeferCleanup(func() {
			run(nil, "systemctl", "--user", "stop", unit)
			run(nil, "machinectl", "terminate", unprivileged)
		})

		// Measured, not assumed: a check that fails is a result, not a failure.
		checks := map[string]bool{}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !checks["boot"]; time.Sleep(500 * time.Millisecond) {
			list, _, _ := run(nil, "machinectl", "list", "--no-pager", "--no-legend")
			checks["boot"] = strings.Contains(list, unprivileged)
		}
		if checks["boot"] {
			out, _, _ := run(nil, "machinectl", "show", unprivileged, "-p", "NetworkInterfaces", "--value")
			checks["veth"] = strings.TrimSpace(out) != ""
			_, _, status = run(nil, "systemd-run", "--user", "-M", unprivileged, "--pipe", "--wait", "--quiet", "--collect", "--", "true")
			checks["entry"] = status == 0
			out, _, status = run(nil, "systemd-run", "--user", "-M", unprivileged, "--pipe", "--wait", "--quiet", "--collect", "-p", "User=agent", "--", "podman", "run", "--rm", hello)
			checks["podman"] = status == 0 && strings.Contains(out, "Hello Podman World")
		}
		avc, _, _ := run(nil, "sudo", "ausearch", "-m", "avc", "-ts", "recent")
		GinkgoWriter.Printf("unprivileged boot: %v\nAVCs:\n%s\n", checks, avc)

		By("the write-up states which of boot, veth, entry and podman worked")
		doc, err := os.ReadFile(writeUp)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(doc)).To(MatchRegexp(`(?m)^\*\*Verdict: E7 (pass|fail), \w+ of four`), "§E7 has no verdict line naming the count")
	})
})
