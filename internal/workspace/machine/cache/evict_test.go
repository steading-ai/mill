package cache_test

import (
	"context"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// Eviction over the fake driver: expiry, then least recently used under
// free space the fake's Statfs scripts (docs/sandbox.md §Eviction). The
// image store is a real one over the same fake, with a temporary machines
// directory, so an image subvolume is a directory the way a generation is.

const (
	day     = 24 * time.Hour
	imgBase = image.Digest("a3f1c2d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")
)

var (
	repoA = cache.Repo("gitlab.com-ivy-a-0000000a")
	repoB = cache.Repo("gitlab.com-ivy-b-0000000b")
	repoC = cache.Repo("gitlab.com-ivy-c-0000000c")
)

// evictFixture is the cache fixture with an image store beside it.
type evictFixture struct {
	*fixture
	images *image.Store
}

func newEvictFixture(configure ...func(*cache.Config)) *evictFixture {
	GinkgoHelper()
	f := &evictFixture{fixture: newFixture(configure...)}
	f.images = image.New(f.fake, image.Config{
		Root:     f.root,
		Machines: GinkgoT().TempDir(),
		Now:      func() time.Time { return f.now },
	})
	return f
}

// lineageUsed makes a lineage with one generation whose last use is `ago`
// before the fixture's clock, with a size estimate.
func (f *evictFixture) lineageUsed(lineage cache.Lineage, ago time.Duration, size uint64) {
	GinkgoHelper()
	was := f.now
	f.now = was.Add(-ago)
	f.generation(lineage, 1, "marker")
	manifest, err := f.store.Manifest(lineage)
	Expect(err).NotTo(HaveOccurred())
	manifest.LastRestoredAt = f.now
	manifest.Generations[0].SizeBytes = size
	Expect(writeManifest(f.store, lineage, manifest)).To(Succeed())
	f.now = was
}

// imageBooted builds an image over the base with the packages and boots it
// `ago` before the fixture's clock.
func (f *evictFixture) imageBooted(ago time.Duration, packages ...string) image.Digest {
	GinkgoHelper()
	was := f.now
	f.now = was.Add(-ago)
	digest, finding, err := f.images.Ensure(context.Background(), imgBase, packages)
	Expect(err).NotTo(HaveOccurred())
	Expect(finding).To(BeNil())
	Expect(f.images.Booted(digest)).To(Succeed())
	f.now = was
	return digest
}

// statfs is a btrfs volume of 100 GiB with the given free space.
func statfs(free uint64) driver.Filesystem {
	return driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: free}
}

// names is what a report deleted, by name and pass.
func names(report cache.Report) []string {
	var out []string
	for _, d := range report.Deleted {
		out = append(out, string(d.Pass)+" "+d.Name())
	}
	return out
}

var _ = Describe("Eviction", Label("unit"), func() {
	var f *evictFixture

	BeforeEach(func() {
		f = newEvictFixture(func(c *cache.Config) {
			c.HighWatermark = cache.Watermark{Percent: 10}
			c.LowWatermark = cache.Watermark{Percent: 20}
		})
	})

	It("Unused lineages expire", func(ctx context.Context) {
		By("given a repo lineage last restored 31 days ago and an expiry of 30")
		stale := cache.Lineage{Scope: repoA, Name: "go-build"}
		f.lineageUsed(stale, 31*day, 1<<20)
		fresh := cache.Lineage{Scope: repoB, Name: "go-build"}
		f.lineageUsed(fresh, day, 1<<20)
		staleImage := f.imageBooted(31*day, "libpq-devel")
		freshImage := f.imageBooted(day, "vips-devel")
		f.fake.Script(driver.Statfs, statfs(50<<30))
		policy := f.store.Policy(f.images)
		policy.Expiry = 30 * day

		By("when eviction runs")
		report, err := f.store.Evict(ctx, policy)
		Expect(err).NotTo(HaveOccurred())

		By("then the lineage is deleted and the report names it")
		Expect(names(report)).To(Equal([]string{
			"expiry " + stale.String(),
			"expiry " + string(staleImage),
		}))
		Expect(report.Deleted[0].Generations).To(Equal([]int{1}))
		Expect(report.Deleted[0].Reason).To(Equal("unused for 31d, past the expiry of 30d"))
		Expect(f.store.GenerationPath(stale, 1)).NotTo(BeAnExistingFile())
		Expect(generations(f.store, stale)).To(BeEmpty())
		manifest, err := f.store.Manifest(stale)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Evicted).NotTo(BeNil())
		Expect(manifest.Evicted.Generations).To(Equal([]int{1}))

		By("and the one restored yesterday is not")
		Expect(f.store.GenerationPath(fresh, 1)).To(BeADirectory())

		By("and the image subvolume last booted 31 days ago is deleted, the fresh one not")
		Expect(f.images.Path(staleImage)).NotTo(BeAnExistingFile())
		Expect(f.images.Path(freshImage)).To(BeADirectory())

		By("and a read-only generation is made writable before its delete, which is synced")
		Expect(f.fake.Calls()).To(ContainElements(
			driver.Call{Method: driver.SetReadOnly, Args: []any{f.store.GenerationPath(stale, 1), false}},
			driver.Call{Method: driver.DeleteSubvolume, Args: []any{f.store.GenerationPath(stale, 1), true}},
			driver.Call{Method: driver.DeleteSubvolume, Args: []any{f.images.Path(staleImage), true}},
		))
	})

	It("Least recently used goes first, until the low watermark", func(ctx context.Context) {
		By("given free space below the high watermark and three repos used 10, 5 and 1 days ago")
		a := cache.Lineage{Scope: repoA, Name: "go-build"}
		b := cache.Lineage{Scope: repoB, Name: "go-build"}
		c := cache.Lineage{Scope: repoC, Name: "go-build"}
		f.lineageUsed(a, 10*day, 1<<30)
		f.lineageUsed(b, 5*day, 1<<30)
		f.lineageUsed(c, day, 1<<30)
		global := cache.Lineage{Scope: cache.Global, Name: "go-mod"}
		f.lineageUsed(global, 40*day, 1<<30)
		// Free space rises by a lineage per deletion: 5 GiB is below the
		// high watermark of 10 GiB, 15 GiB is between the two, 25 GiB is
		// above the low watermark of 20 GiB.
		f.fake.Script(driver.Statfs, statfs(5<<30), statfs(15<<30), statfs(25<<30), statfs(35<<30))

		By("when eviction runs")
		report, err := f.store.Evict(ctx, f.store.Policy(f.images).WatermarkOnly())
		Expect(err).NotTo(HaveOccurred())

		By("then the oldest repo's lineages are deleted first")
		Expect(names(report)).To(Equal([]string{"watermark " + a.String(), "watermark " + b.String()}))
		Expect(report.Deleted[0].Reason).To(ContainSubstring("free space 5G below the high watermark 10% (10G of 100G)"))
		Expect(f.store.GenerationPath(a, 1)).NotTo(BeAnExistingFile())

		By("and eviction stops when free space exceeds the low watermark")
		Expect(f.store.GenerationPath(c, 1)).To(BeADirectory(), "the most recently used survives")
		Expect(f.fake.CallsTo(driver.Statfs)).To(HaveLen(3), "the first read, then one after each deletion")
		Expect(report.Free).To(Equal(uint64(25 << 30)))
		Expect(report.Short).To(BeFalse())

		By("and the global lineage, oldest of all, is never a victim")
		Expect(f.store.GenerationPath(global, 1)).To(BeADirectory())
		for _, call := range f.fake.CallsTo(driver.DeleteSubvolume) {
			Expect(call.Args[0].(string)).NotTo(HavePrefix(filepath.Join(f.store.LineageRoot(), "global")))
		}

		By("and the driver's sync is called after every deletion and before the next Statfs")
		calls := f.fake.Calls()
		for i, call := range calls {
			if call.Method == driver.DeleteSubvolume {
				Expect(call.Args[1]).To(BeTrue(), "delete %s without sync", call.Args[0])
			}
			if call.Method == driver.Statfs && i > 0 {
				previous := calls[i-1]
				Expect(previous.Method).To(Equal(driver.DeleteSubvolume), "a re-read of free space follows a synced deletion, not %s", previous.Method)
				Expect(previous.Args[1]).To(BeTrue())
			}
		}
	})

	It("Free space, not estimates, decides", func(ctx context.Context) {
		By("given estimates summing above the high watermark but statfs above it")
		a := cache.Lineage{Scope: repoA, Name: "go-build"}
		b := cache.Lineage{Scope: repoB, Name: "go-build"}
		f.lineageUsed(a, 10*day, 8<<30)
		f.lineageUsed(b, 5*day, 8<<30)
		f.imageBooted(3*day, "libpq-devel")
		f.fake.Script(driver.Statfs, statfs(30<<30))

		By("when eviction runs")
		report, err := f.store.Evict(ctx, f.store.Policy(f.images))
		Expect(err).NotTo(HaveOccurred())

		By("then nothing is evicted")
		Expect(report.Deleted).To(BeEmpty())
		Expect(f.fake.CallsTo(driver.DeleteSubvolume)).To(BeEmpty())
		Expect(f.store.GenerationPath(a, 1)).To(BeADirectory())
		Expect(f.store.GenerationPath(b, 1)).To(BeADirectory())
		Expect(report.Free).To(Equal(uint64(30 << 30)))
		Expect(report.High).To(Equal(uint64(10 << 30)))
	})

	It("A run in flight is untouched", func(ctx context.Context) {
		By("given a run restored generation 2 of a lineage")
		f.generation(goBuild, 1, "old")
		f.generation(goBuild, 2, "marker")
		restored, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Generation).To(Equal(2))
		f.write(runA, "go-build", "written-by-the-run")

		By("when eviction deletes the lineage")
		f.fake.Script(driver.Statfs, statfs(5<<30), statfs(25<<30))
		report, err := f.store.Evict(ctx, f.store.Policy(nil).WatermarkOnly())
		Expect(err).NotTo(HaveOccurred())
		Expect(names(report)).To(Equal([]string{"watermark " + goBuild.String()}))
		Expect(report.Deleted[0].Generations).To(Equal([]int{1, 2}))
		Expect(generations(f.store, goBuild)).To(BeEmpty())

		By("then the run's copy is intact")
		Expect(files(f.store.CopyPath(runA, "go-build"))).To(ConsistOf("marker", "written-by-the-run"))
		for _, call := range f.fake.CallsTo(driver.DeleteSubvolume) {
			Expect(call.Args[0].(string)).NotTo(HavePrefix(f.store.RunDir(runA)), "a run's copy is never touched")
		}

		By("and the run promotes into a fresh lineage")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted).To(HaveLen(1))
		Expect(promoted[0].Generation).To(Equal(1))
		Expect(generations(f.store, goBuild)).To(Equal([]int{1}))
		Expect(files(f.store.GenerationPath(goBuild, 1))).To(ConsistOf("marker", "written-by-the-run"))
		manifest, err := f.store.Manifest(goBuild)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Evicted).To(BeNil(), "the promotion clears the eviction note")
	})

	It("refuses a low watermark below the high one", func(ctx context.Context) {
		f.fake.Script(driver.Statfs, statfs(5<<30))
		policy := f.store.Policy(nil).WatermarkOnly()
		policy.High, policy.Low = cache.Watermark{Percent: 20}, cache.Watermark{Percent: 10}
		_, err := f.store.Evict(ctx, policy)
		Expect(err).To(MatchError(ContainSubstring("low watermark 10% is below the high watermark 20%")))
	})

	It("reports when the victims run out before the low watermark", func(ctx context.Context) {
		a := cache.Lineage{Scope: repoA, Name: "go-build"}
		f.lineageUsed(a, 10*day, 1<<30)
		f.fake.Script(driver.Statfs, statfs(5<<30), statfs(6<<30))
		report, err := f.store.Evict(ctx, f.store.Policy(nil).WatermarkOnly())
		Expect(err).NotTo(HaveOccurred())
		Expect(names(report)).To(Equal([]string{"watermark " + a.String()}))
		Expect(report.Short).To(BeTrue())
	})

	It("leaves an evicted lineage's directory, lock and manifest for the run that restored it", func(ctx context.Context) {
		a := cache.Lineage{Scope: repoA, Name: "go-build"}
		f.lineageUsed(a, 31*day, 1<<20)
		_, err := f.store.Evict(ctx, f.store.Policy(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Join(f.store.Dir(a), cache.ManifestName)).To(BeARegularFile())
		Expect(filepath.Join(f.store.Dir(a), cache.LockName)).To(BeARegularFile())
		summaries, err := f.store.List()
		Expect(err).NotTo(HaveOccurred())
		Expect(summaries).To(HaveLen(1))
		Expect(summaries[0].Evicted).NotTo(BeNil())
		Expect(summaries[0].Generations).To(BeEmpty())

		By("and a second pass has nothing to delete there")
		report, err := f.store.Evict(ctx, f.store.Policy(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Deleted).To(BeEmpty())
	})

	It("a per-lineage global cap applies at promotion and in the listing", func(ctx context.Context) {
		f = newEvictFixture(func(c *cache.Config) {
			c.GlobalCap = 1 << 30
			c.GlobalCaps = map[string]uint64{"mise": 8 << 30}
		})
		Expect(f.store.Config().Cap(cache.Lineage{Scope: cache.Global, Name: "mise"})).To(Equal(uint64(8 << 30)))
		Expect(f.store.Config().Cap(goMod)).To(Equal(uint64(1 << 30)))
		Expect(f.store.Config().Cap(goBuild)).To(Equal(uint64(cache.DefaultRepoCap)))

		mise := cache.Lineage{Scope: cache.Global, Name: "mise"}
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{mise})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "mise", "tool")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())
		f.fake.Script(driver.DiskUsage, uint64(4<<30))
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted[0].Reset).To(BeFalse(), "4G is under mise's own cap of 8G, over the general 1G")
		Expect(promoted[0].Generation).To(Equal(1))
	})
})

var _ = Describe("Watermarks", Label("unit"), func() {
	It("parse as a share of the volume or a size", func() {
		share, err := cache.ParseWatermark("10%")
		Expect(err).NotTo(HaveOccurred())
		Expect(share).To(Equal(cache.Watermark{Percent: 10}))
		Expect(share.Of(statfs(0))).To(Equal(uint64(10 << 30)))
		Expect(share.String()).To(Equal("10%"))

		size, err := cache.ParseWatermark("50G")
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(Equal(cache.Watermark{Bytes: 50 << 30}))
		Expect(size.Of(statfs(0))).To(Equal(uint64(50 << 30)))
		Expect(size.String()).To(Equal("50G"))

		for _, bad := range []string{"", "0%", "150%", "soon", "-1G"} {
			_, err := cache.ParseWatermark(bad)
			Expect(err).To(HaveOccurred(), bad)
		}
	})

	It("default to a tenth and a fifth of the volume", func() {
		config := cache.New(driver.NewFake(), cache.Config{Root: GinkgoT().TempDir()}).Config()
		Expect(config.HighWatermark).To(Equal(cache.Watermark{Percent: 10}))
		Expect(config.LowWatermark).To(Equal(cache.Watermark{Percent: 20}))
		Expect(config.Expiry).To(Equal(30 * day))
	})
})
