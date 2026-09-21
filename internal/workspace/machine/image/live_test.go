package image_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// The live tier: the base built from images/base by the worker's own
// pipeline on a host with podman, sudo and the sandbox's btrfs volume, and
// derived images built, exported and degraded through the real systemd
// driver. Off unless MILL_SANDBOX_HOST says so (docs/sandbox.md §Testing).
//
// Nothing here touches /var/lib/machines: the subvolumes land in a
// directory of the spec's own under the sandbox root, created and deleted
// with the run. The base is tagged under Harbor's name so the derived
// recipe's FROM resolves it from local storage, and is never pushed.

const (
	liveVar        = "MILL_SANDBOX_HOST"
	liveRoot       = "/var/lib/mill"
	liveMachines   = driver.DefaultMachines
	liveRegistry   = image.DefaultRegistry
	liveRepository = "mill/base-live"
	livePackage    = "libpq-devel"
	missingPackage = "libfooo-devel"
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

// must runs argv on the host and fails the spec unless it exits zero.
func must(argv ...string) string {
	GinkgoHelper()
	out, stderr, status := host(argv...)
	Expect(status).To(BeZero(), "%s: %s", strings.Join(argv, " "), stderr)
	return out
}

// inImage runs a shell command in a container of the image, as root.
func inImage(tag, script string) (string, int) {
	GinkgoHelper()
	out, stderr, status := host("podman", "run", "--rm", tag, "sh", "-c", script)
	return out + stderr, status
}

// repoRoot is this repository's checkout, where images/base is.
func repoRoot() string {
	GinkgoHelper()
	dir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		Expect(parent).NotTo(Equal(dir), "no go.mod above %s", dir)
		dir = parent
	}
}

// countingDriver is the real driver with its builds counted.
type countingDriver struct {
	driver.Driver
	builds atomic.Int32
}

func (d *countingDriver) BuildImageOnHost(ctx context.Context, build driver.ImageBuild) (string, error) {
	d.builds.Add(1)
	return d.Driver.BuildImageOnHost(ctx, build)
}

var _ = Describe("Images on a real host", Label("live"), Serial, Ordered, func() {
	var (
		counted  *countingDriver
		store    *image.Store
		machines string
		base     image.Digest
		tags     []string
	)

	BeforeAll(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, podman and the sandbox's btrfs volume to run the spec that builds real images")
		}
		if testsupport.InsideMachine() {
			Skip(testsupport.HostOnlySkip("the host's podman and image volume to build and export an image"))
		}
		real := driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: "sudo"})
		testsupport.SkipUnlessSandboxReady(ctx, real, liveRoot, liveMachines)
		if _, err := exec.LookPath("podman"); err != nil {
			Skip("podman is not on PATH; the base and derived images need it")
		}
		if _, _, status := host("sudo", "-n", "true"); status != 0 {
			Skip("sudo -n is refused; the export writes the image tree as root")
		}
		var err error
		machines, err = os.MkdirTemp(liveRoot, "image-live-*")
		Expect(err).NotTo(HaveOccurred(), "the sandbox root %s is not this account's", liveRoot)
		DeferCleanup(func() {
			entries, _ := filepath.Glob(filepath.Join(machines, image.Prefix+"*"))
			for _, entry := range entries {
				host("sudo", "-n", "btrfs", "subvolume", "delete", entry)
			}
			host("sudo", "-n", "rm", "-rf", machines)
			for _, tag := range tags {
				host("podman", "rmi", tag)
			}
		})

		counted = &countingDriver{Driver: real}
		store = image.New(counted, image.Config{
			Root:        GinkgoT().TempDir(),
			Machines:    machines,
			Registry:    liveRegistry,
			Repository:  liveRepository,
			BuildBudget: 10 * time.Minute,
		})

		By("building the base from images/base with the worker's build verb")
		result, err := store.BuildBase(ctx, image.BaseBuild{Context: filepath.Join(repoRoot(), "images", "base")})
		Expect(err).NotTo(HaveOccurred())
		tags = result.Tags
		base = result.Digest
		counted.builds.Store(0)
	})

	It("The base carries the session user and the shim and nothing of the operator's", func(ctx context.Context) {
		By("when the base image is built")
		tag := tags[1]

		By("then agent exists with uid 1000 and the shim is the only sudo path")
		out, status := inImage(tag, "id -u agent")
		Expect(status).To(BeZero(), out)
		Expect(strings.TrimSpace(out)).To(Equal("1000"))
		out, status = inImage(tag, "test -x /usr/local/bin/dnf && head -1 /usr/local/bin/dnf && sudo -l -U agent")
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("#!/bin/sh"))
		Expect(out).To(ContainSubstring("(root) NOPASSWD: /usr/bin/dnf install *"))
		Expect(strings.Count(out, "NOPASSWD")).To(Equal(1), "one sudo rule and no other:\n%s", out)
		out, status = inImage(tag, "cat /etc/sudoers.d/agent-dnf; ls /etc/sudoers.d")
		Expect(status).To(BeZero(), out)
		Expect(strings.Fields(out)).To(Equal([]string{"agent", "ALL=(root)", "NOPASSWD:", "/usr/bin/dnf", "install", "*", "agent-dnf"}))

		By("and podman.socket is enabled, containers.conf splits cgroups, and the subordinate ids fit the machine")
		out, status = inImage(tag, `systemctl is-enabled podman.socket mill-proc.service; grep -h cgroups /etc/containers/containers.conf; cat /etc/subuid /etc/subgid; grep -c '^location = ' /etc/containers/registries.conf.d/harbor.conf`)
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("enabled\nenabled\n"))
		Expect(out).To(ContainSubstring(`cgroups = "split"`))
		Expect(out).To(ContainSubstring("agent:10000:55536\nagent:10000:55536\n"))
		Expect(out).To(HaveSuffix("3\n"), "docker.io, quay.io and ghcr.io each name a proxy project")

		By("and no credential file is present")
		out, status = inImage(tag, `find / -xdev \( -name .credentials.json -o -name claude-oauth -o -name harbor-robot \) 2>/dev/null; ls -d /home/agent/.claude 2>&1 || true`)
		Expect(status).To(BeZero(), out)
		Expect(out).NotTo(ContainSubstring(".credentials.json"))
		Expect(out).NotTo(ContainSubstring("claude-oauth"))
		Expect(out).To(ContainSubstring("No such file"), "no skill directory either")

		By("and the exported tree has its capabilities back")
		digest, finding, err := store.Ensure(ctx, base, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(finding).To(BeNil())
		Expect(digest).To(Equal(base))
		tree := store.Path(base)
		caps := must("sudo", "-n", "getcap", filepath.Join(tree, "usr/bin/newuidmap"), filepath.Join(tree, "usr/bin/newgidmap"))
		Expect(caps).To(ContainSubstring("newuidmap cap_setuid=ep"))
		Expect(caps).To(ContainSubstring("newgidmap cap_setgid=ep"))
		Expect(must("sudo", "-n", "stat", "-c", "%u %a", filepath.Join(tree, "etc/sudoers.d/agent-dnf"))).To(Equal("0 440\n"), "root-owned, as sudo requires")
		Expect(must("sudo", "-n", "cat", filepath.Join(tree, "etc/machine-id"))).To(BeEmpty())
	})

	It("A missing derived image is built once", func(ctx context.Context) {
		By("given no subvolume for the digest")
		digest := image.Derived(base, []string{livePackage})
		present, err := counted.Exists(ctx, store.Path(digest))
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeFalse())
		counted.builds.Store(0)

		By("when two runs of the repo are provisioned at once")
		var wg sync.WaitGroup
		results := make([]image.Digest, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				results[i], _, errs[i] = store.Ensure(ctx, base, []string{livePackage})
			}()
		}
		wg.Wait()

		By("then the second waits on the lock and then finds the subvolume")
		Expect(errs).To(Equal([]error{nil, nil}))
		Expect(results).To(Equal([]image.Digest{digest, digest}))
		Expect(counted.builds.Load()).To(Equal(int32(1)), "one build for two callers")
		Expect(must("sudo", "-n", "rpm", "--root", store.Path(digest), "-q", livePackage)).To(HavePrefix(livePackage + "-"))
		_, _, status := host("sudo", "-n", "test", "-e", store.Path(digest)+image.BuildingSuffix)
		Expect(status).NotTo(BeZero(), "no .building remains")
		manifest, err := store.Manifest(digest)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Packages).To(Equal([]string{livePackage}))
		Expect(manifest.Base).To(Equal(base))
	})

	It("A package that does not exist degrades to base with a finding", func(ctx context.Context) {
		By("given mill.toml declares libfooo-devel")
		derived := image.Derived(base, []string{missingPackage})

		By("when the image is ensured")
		digest, finding, err := store.Ensure(ctx, base, []string{missingPackage})
		Expect(err).NotTo(HaveOccurred())

		By("then the run boots from base")
		Expect(digest).To(Equal(base))
		present, err := counted.Exists(ctx, store.Path(base))
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeTrue())
		for _, leftover := range []string{store.Path(derived), store.Path(derived) + image.BuildingSuffix} {
			_, _, status := host("sudo", "-n", "test", "-e", leftover)
			Expect(status).NotTo(BeZero(), "%s remains", leftover)
		}
		images := must("podman", "images", "--format", "{{.Repository}}:{{.Tag}}")
		Expect(images).NotTo(ContainSubstring(string(derived)), "no built image for the failed digest")

		By("and the first session receives a finding naming libfooo-devel and dnf's error")
		Expect(finding).NotTo(BeNil())
		Expect(finding.Packages).To(Equal([]string{missingPackage}))
		Expect(finding.Output).To(ContainSubstring("No match for argument: " + missingPackage))
		Expect(finding.String()).To(ContainSubstring(missingPackage))
	})
})
