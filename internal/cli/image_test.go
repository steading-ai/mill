package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// The image verbs run over the systemd driver with `--sudo none`, so a
// temporary machines directory of plain directories — what the fake
// driver's subvolume verbs leave behind — stands in for the host's.

const specBase = image.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

var _ = Describe("mill image", Label("unit"), func() {
	var stdout, stderr bytes.Buffer
	var root, machines string
	var now time.Time
	var store *image.Store

	run := func(args ...string) int {
		stdout.Reset()
		stderr.Reset()
		return cli.Run(append([]string{"image"}, args...), strings.NewReader(""), &stdout, &stderr)
	}

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		machines = GinkgoT().TempDir()
		now = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		store = image.New(machine.NewFake(), image.Config{Root: root, Machines: machines, Now: func() time.Time { return now }})
		GinkgoT().Setenv("MILL_CONFIG", "")
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	flags := func(extra ...string) []string {
		return append([]string{"--sandbox-root", root, "--machines-dir", machines, "--sudo", "none"}, extra...)
	}

	It("is wired into the tree with its three verbs", func() {
		Expect(run("--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("build-base"))
		Expect(stdout.String()).To(ContainSubstring("ls"))
		Expect(stdout.String()).To(ContainSubstring("prune"))
		Expect(run("build-base", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--push"))
		Expect(stdout.String()).To(ContainSubstring("--robot-credential-file"))
	})

	It("ls lists a built subvolume with its base and packages", func(ctx context.Context) {
		digest, finding, err := store.Ensure(ctx, specBase, []string{"vips-devel", "libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(finding).To(BeNil())
		now = now.Add(time.Hour)
		Expect(store.Booted(digest)).To(Succeed())

		Expect(run(flags("ls")...)).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("DIGEST"))
		Expect(stdout.String()).To(ContainSubstring(digest.Short()))
		Expect(stdout.String()).To(ContainSubstring(specBase.Short()))
		Expect(stdout.String()).To(ContainSubstring("libpq-devel,vips-devel"))
		Expect(stdout.String()).To(ContainSubstring("2026-09-11T13:00:00Z"))
	})

	It("ls says so when there is nothing", func() {
		Expect(run(flags("ls")...)).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("no image subvolumes under " + machines))
	})

	It("prune lists what would go under --dry-run and removes nothing", func(ctx context.Context) {
		// The command's clock is the wall clock, so the ages are real.
		now = time.Now().Add(-40 * 24 * time.Hour)
		old, _, err := store.Ensure(ctx, specBase, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		now = time.Now()
		fresh, _, err := store.Ensure(ctx, specBase, []string{"vips-devel"})
		Expect(err).NotTo(HaveOccurred())

		Expect(run(flags("prune", "--dry-run")...)).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring(old.Short()), stdout.String())
		Expect(stdout.String()).To(ContainSubstring("would remove"))
		Expect(stdout.String()).NotTo(ContainSubstring(fresh.Short()))
		Expect(store.Path(old)).To(BeADirectory())
	})

	It("refuses an expiry that is not a window", func() {
		Expect(run(flags("prune", "--expiry", "soon")...)).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring("--expiry"))
	})

	It("reads the machines directory and the registry from the configuration file", func() {
		config := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(config, []byte("machines-dir: "+machines+"\nsandbox-root: "+root+"\n"), 0o600)).To(Succeed())
		Expect(run("--config", config, "--sudo", "none", "ls")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring(machines))
	})
})
