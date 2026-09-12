package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/workspace"
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

	It("is wired into the tree with ls and prune", func() {
		Expect(run("--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("ls"))
		Expect(stdout.String()).To(ContainSubstring("prune"))
		Expect(stdout.String()).To(ContainSubstring("--global-cap"))
		Expect(run("prune", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--expiry"))
		Expect(stdout.String()).To(ContainSubstring("--high-watermark"))
		Expect(stdout.String()).To(ContainSubstring("--low-watermark"))
	})

	It("prune refuses a watermark that is not a share or a size, and an expiry that is not a window", func() {
		Expect(run("prune", "--sandbox-root", root, "--sudo", "none", "--high-watermark", "lots")).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring("--high-watermark"))
		Expect(run("prune", "--sandbox-root", root, "--sudo", "none", "--expiry", "soon")).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring("--expiry"))
	})

	It("prune reads the expiry and the watermarks from the configuration file, and refuses an inverted pair", func() {
		config := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(config, []byte("sandbox-root: "+root+"\nmachines-dir: "+GinkgoT().TempDir()+
			"\ncache-expiry: 7d\ncache-high-watermark: 20%\ncache-low-watermark: 10%\n"), 0o600)).To(Succeed())

		Expect(run("--config", config, "prune", "--sudo", "none")).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring("low watermark 10% is below the high watermark 20%"))

		By("and a flag wins over the file")
		Expect(run("--config", config, "prune", "--sudo", "none", "--low-watermark", "30%")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("nothing to evict under " + filepath.Join(root, "cache")))
		Expect(stdout.String()).To(ContainSubstring("free "))
	})

	It("ls flags a repo lineage eviction emptied, and a repo lineage over its advisory cap without acting on it", func(ctx context.Context) {
		repo := cache.Lineage{Scope: cache.Repo("gitlab.com-ivy-mill-0a1b2c3d"), Name: "go-build"}
		promote(ctx, "01ONE", repo, 12<<30)
		gone := cache.Lineage{Scope: cache.Repo("gitlab.com-ivy-old-0a1b2c3d"), Name: "go-build"}
		now = now.Add(-40 * 24 * time.Hour)
		promote(ctx, "01OLD", gone, 1<<20)
		now = now.Add(40 * 24 * time.Hour)
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 50 << 30})
		report, err := store.Evict(ctx, store.Policy(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Deleted).To(HaveLen(1), "the expired one goes; the one over its cap is a flag, not a victim")
		Expect(report.Deleted[0].Lineage).To(Equal(gone))

		Expect(run("ls", "--sandbox-root", root, "--repo-cap", "10G")).To(BeZero(), stderr.String())
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		Expect(lines).To(HaveLen(3), stdout.String())
		Expect(strings.Fields(lines[1])).To(Equal([]string{"repo/gitlab.com-ivy-mill-0a1b2c3d", "go-build", "1", "12G", "2026-09-11T12:00:00Z", "over-cap"}))
		Expect(strings.Fields(lines[2])).To(Equal([]string{"repo/gitlab.com-ivy-old-0a1b2c3d", "go-build", "-", "-", "2026-08-02T12:00:00Z", "evicted"}))
		Expect(store.GenerationPath(repo, 1)).To(BeADirectory())
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

// The live tier: `mill cache prune` against the sandbox's btrfs volume as
// the worker's own uid, on a root of the spec's own under it. Off unless
// MILL_SANDBOX_HOST says so (docs/sandbox.md §Testing).
var _ = Describe("mill cache prune on the sandbox volume", Label("live"), Serial, func() {
	It("deletes an expired lineage and names it", func(ctx context.Context) {
		if os.Getenv("MILL_SANDBOX_HOST") == "" {
			Skip("set MILL_SANDBOX_HOST=1 on a host with root, nspawn and btrfs to run the spec that prunes real lineages")
		}
		host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
		// The sandbox root this process prunes under: the host's volume, or
		// — inside a machine, where mill's own live tier runs once mill
		// builds mill — the nested worker's subvolume under the ephemeral
		// root, made as the worker's own uid the way `mill run` would
		// (docs/sandbox.md §Self-hosting, "The inner worker's root").
		sandbox := "/var/lib/mill"
		if manager, err := host.Container(ctx); err == nil && manager == driver.ContainerNspawn {
			Expect(driver.EnsureNestedRoot(ctx, host, driver.NestedRoot)).To(Succeed())
			sandbox = driver.NestedRoot
		}
		root, err := os.MkdirTemp(sandbox, "cache-prune-live-*")
		Expect(err).NotTo(HaveOccurred(), "%s is not this account's", sandbox)
		store := cache.New(host, cache.Config{Root: root})
		DeferCleanup(func(ctx context.Context) {
			for _, lineage := range []cache.Lineage{{Scope: cache.Repo("live-a"), Name: "expired"}, {Scope: cache.Repo("live-a"), Name: "fresh"}} {
				manifest, _ := store.Manifest(lineage)
				for _, g := range manifest.Generations {
					_ = host.SetReadOnly(ctx, store.GenerationPath(lineage, g.Number), false)
					_ = host.DeleteSubvolume(ctx, store.GenerationPath(lineage, g.Number), true)
				}
			}
			_ = os.RemoveAll(root)
		})
		fs, err := host.Statfs(root)
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.Btrfs()).To(BeTrue(), "%s is %s, not btrfs", root, fs.Type)

		By("given a repo lineage last restored 31 days ago and one restored today")
		seed := func(id string, lineage cache.Lineage) {
			GinkgoHelper()
			_, err := store.Restore(ctx, id, []cache.Lineage{lineage})
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(store.CopyPath(id, lineage.Name), "f"), []byte("x"), 0o644)).To(Succeed())
			Expect(store.Candidate(ctx, id, "build")).To(Succeed())
			_, err = store.Promote(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.Discard(ctx, id)).To(Succeed())
		}
		expired := cache.Lineage{Scope: cache.Repo("live-a"), Name: "expired"}
		fresh := cache.Lineage{Scope: cache.Repo("live-a"), Name: "fresh"}
		seed("01LIVEEXPIRED", expired)
		seed("01LIVEFRESH", fresh)
		manifest, err := store.Manifest(expired)
		Expect(err).NotTo(HaveOccurred())
		manifest.LastRestoredAt = time.Now().Add(-31 * 24 * time.Hour)
		manifest.Generations[0].CreatedAt = manifest.LastRestoredAt
		Expect(os.WriteFile(filepath.Join(store.Dir(expired), cache.ManifestName), mustJSON(manifest), 0o644)).To(Succeed())

		By("when mill cache prune runs on the real volume")
		var stdout, stderr bytes.Buffer
		status := cli.Run([]string{"cache", "prune", "--sandbox-root", root, "--machines-dir", GinkgoT().TempDir(), "--sudo", "none",
			"--expiry", "30d", "--high-watermark", "1K", "--low-watermark", "2K"}, strings.NewReader(""), &stdout, &stderr)
		Expect(status).To(BeZero(), stderr.String())

		By("then it deleted the expired lineage and names it")
		Expect(stdout.String()).To(ContainSubstring("lineage  "+expired.String()+" gen-1"), stdout.String())
		Expect(stdout.String()).To(ContainSubstring("past the expiry of 30d"))
		Expect(stdout.String()).NotTo(ContainSubstring(fresh.String()))
		Expect(stdout.String()).To(MatchRegexp(`free \S+ of \S+`), "free space read from the volume")
		Expect(store.GenerationPath(expired, 1)).NotTo(BeAnExistingFile())
		Expect(store.GenerationPath(fresh, 1)).To(BeADirectory())
	})
})

func mustJSON(value any) []byte {
	GinkgoHelper()
	raw, err := json.MarshalIndent(value, "", "  ")
	Expect(err).NotTo(HaveOccurred())
	return raw
}
