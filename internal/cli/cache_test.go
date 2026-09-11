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
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The cache verbs read the directory tree and each lineage's manifest, so a
// temporary root filled through a store over the fake driver — where a
// subvolume is a directory — is what the command lists.

var _ = Describe("mill cache", Label("unit"), func() {
	var stdout, stderr bytes.Buffer
	var root string
	var now time.Time
	var store *cache.Store
	var fake *driver.FakeDriver

	run := func(args ...string) int {
		stdout.Reset()
		stderr.Reset()
		return cli.Run(append([]string{"cache"}, args...), strings.NewReader(""), &stdout, &stderr)
	}

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		now = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		fake = driver.NewFake()
		store = cache.New(fake, cache.Config{Root: root, GlobalCap: 1 << 30, Now: func() time.Time { return now }})
		GinkgoT().Setenv("MILL_CONFIG", "")
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	// promote runs one promoting run of a lineage whose candidate du
	// reports at size.
	promote := func(ctx context.Context, id string, lineage cache.Lineage, size uint64) {
		GinkgoHelper()
		_, err := store.Restore(ctx, id, []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(store.CopyPath(id, lineage.Name), "f"), []byte("x"), 0o644)).To(Succeed())
		Expect(store.Candidate(ctx, id, "build")).To(Succeed())
		fake.Script(driver.DiskUsage, size)
		_, err = store.Promote(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Discard(ctx, id)).To(Succeed())
	}

	It("is wired into the tree with ls", func() {
		Expect(run("--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("ls"))
		Expect(stdout.String()).To(ContainSubstring("--global-cap"))
	})

	It("ls lists scope, name, generations, latest size estimate, last-restored and the over-cap flag", func(ctx context.Context) {
		repo := cache.Lineage{Scope: cache.Repo("gitlab.com-ivy-mill-0a1b2c3d"), Name: "go-build"}
		promote(ctx, "01ONE", repo, 3<<20)
		promote(ctx, "01TWO", repo, 5<<20)
		now = now.Add(time.Hour)
		promote(ctx, "01THREE", repo, 12<<30)
		global := cache.Lineage{Scope: cache.Global, Name: "go-mod"}
		promote(ctx, "01FOUR", global, 2<<30)
		_, err := store.Restore(ctx, "01FIVE", []cache.Lineage{{Scope: cache.Global, Name: "mise"}})
		Expect(err).NotTo(HaveOccurred())

		Expect(run("ls", "--sandbox-root", root, "--global-cap", "1G", "--repo-cap", "10G")).To(BeZero(), stderr.String())

		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		Expect(lines).To(HaveLen(4), stdout.String())
		Expect(strings.Fields(lines[0])).To(Equal([]string{"SCOPE", "NAME", "GENERATIONS", "LATEST", "SIZE", "LAST", "RESTORED", "FLAGS"}))
		Expect(strings.Fields(lines[1])).To(Equal([]string{"global", "go-mod", "-", "-", "2026-09-11T13:00:00Z", "reset"}), "over its cap, so reset")
		Expect(strings.Fields(lines[2])).To(Equal([]string{"global", "mise", "-", "-", "2026-09-11T13:00:00Z", "-"}))
		Expect(strings.Fields(lines[3])).To(Equal([]string{"repo/gitlab.com-ivy-mill-0a1b2c3d", "go-build", "2,3", "12G", "2026-09-11T13:00:00Z", "over-cap"}))
	})

	It("ls says so when there is nothing", func() {
		Expect(run("ls", "--sandbox-root", root)).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("no lineages under " + filepath.Join(root, "cache")))
	})

	It("reads the sandbox root and the caps from the configuration file, and refuses a cap that is not a size", func(ctx context.Context) {
		promote(ctx, "01ONE", cache.Lineage{Scope: cache.Global, Name: "go-mod"}, 512<<20)
		config := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(config, []byte("sandbox-root: "+root+"\ncache-global-cap: 256M\n"), 0o600)).To(Succeed())

		Expect(run("--config", config, "ls")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("512M"))
		Expect(stdout.String()).To(ContainSubstring("over-cap"))

		Expect(run("--config", config, "ls", "--repo-cap", "lots")).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring("--repo-cap"))
	})
})
