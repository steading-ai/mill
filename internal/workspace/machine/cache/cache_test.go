package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/config"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The lineage over the fake driver, where a subvolume is a directory and a
// snapshot is a copy: every rule of docs/sandbox.md §Caching that does not
// depend on btrfs itself. What does — that a snapshot shares its extents
// and costs nothing, that a read-only generation refuses its owner's
// delete — is the live spec's.

const (
	runA   = "01RUNA"
	runB   = "01RUNB"
	repoID = "gitlab.com-ivy-mill-0a1b2c3d"
)

var (
	repoScope = cache.Repo(repoID)
	goBuild   = cache.Lineage{Scope: repoScope, Name: "go-build"}
	goMod     = cache.Lineage{Scope: cache.Global, Name: "go-mod"}
)

// fixture is a store over the fake driver under a temporary root, with a
// clock the spec advances.
type fixture struct {
	fake  *driver.FakeDriver
	store *cache.Store
	root  string
	now   time.Time
}

func newFixture(configure ...func(*cache.Config)) *fixture {
	GinkgoHelper()
	f := &fixture{
		fake: driver.NewFake(),
		root: GinkgoT().TempDir(),
		now:  time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
	}
	cfg := cache.Config{Root: f.root, Now: func() time.Time { return f.now }}
	for _, c := range configure {
		c(&cfg)
	}
	f.store = cache.New(f.fake, cfg)
	return f
}

// generation makes gen-<n> of a lineage holding the named files, the way a
// promotion would have left it, and records it in the manifest.
func (f *fixture) generation(lineage cache.Lineage, n int, files ...string) {
	GinkgoHelper()
	dir := f.store.GenerationPath(lineage, n)
	Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
	for _, name := range files {
		Expect(os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644)).To(Succeed())
	}
	manifest, err := f.store.Manifest(lineage)
	Expect(err).NotTo(HaveOccurred())
	for i := range manifest.Generations {
		if manifest.Generations[i].Number == n {
			manifest.Generations[i] = cache.Generation{Number: n, CreatedAt: f.now, SizeBytes: uint64(len(files)) * 8, Run: "01SEED", Step: "seed"}
		}
	}
	Expect(writeManifest(f.store, lineage, manifest)).To(Succeed())
}

// writeManifest writes a lineage's manifest the way the store does, by
// promoting nothing: the store has no public write, so the spec goes
// through the file.
func writeManifest(store *cache.Store, lineage cache.Lineage, manifest cache.Manifest) error {
	return writeJSON(filepath.Join(store.Dir(lineage), cache.ManifestName), manifest)
}

// write puts a file in the run's copy of a lineage, as a session would
// through the bind.
func (f *fixture) write(run, name, file string) {
	GinkgoHelper()
	Expect(os.WriteFile(filepath.Join(f.store.CopyPath(run, name), file), []byte(file+"\n"), 0o644)).To(Succeed())
}

// files is the regular files under a directory, relative to it.
func files(root string) []string {
	GinkgoHelper()
	var out []string
	Expect(filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})).To(Succeed())
	return out
}

func generations(store *cache.Store, lineage cache.Lineage) []int {
	GinkgoHelper()
	manifest, err := store.Manifest(lineage)
	Expect(err).NotTo(HaveOccurred())
	var out []int
	for _, g := range manifest.Generations {
		out = append(out, g.Number)
	}
	return out
}

var _ = Describe("Cache lineages", Label("unit"), func() {
	var f *fixture

	BeforeEach(func() { f = newFixture() })

	It("The latest generation restores and the run's writes do not touch it", func(ctx context.Context) {
		By("given lineage go-build with generation 2 containing marker")
		f.generation(goBuild, 1, "old")
		f.generation(goBuild, 2, "marker")

		By("when a run restores it and the session writes to the cache path")
		restored, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "written-by-the-run")

		By("then marker is present in the run's copy")
		Expect(restored).To(HaveLen(1))
		Expect(restored[0].Generation).To(Equal(2))
		Expect(files(f.store.CopyPath(runA, "go-build"))).To(ConsistOf("marker", "written-by-the-run"))
		Expect(f.fake.CallsTo(driver.Snapshot)).To(Equal([]driver.Call{{
			Method: driver.Snapshot,
			Args:   []any{f.store.GenerationPath(goBuild, 2), f.store.CopyPath(runA, "go-build"), false},
		}}), "one writable snapshot of gen-2, whatever its size")

		By("and generation 2 is unchanged")
		Expect(files(f.store.GenerationPath(goBuild, 2))).To(Equal([]string{"marker"}))
		manifest, err := f.store.Manifest(goBuild)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.LastRestoredAt).To(Equal(f.now))
		Expect(manifest.LastRestoredBy).To(Equal(runA))
	})

	It("A lineage with no generations restores an empty writable subvolume", func(ctx context.Context) {
		restored, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())

		Expect(restored[0].Generation).To(BeZero())
		Expect(restored[0].Reason).To(Equal("no generation"))
		Expect(f.store.CopyPath(runA, "go-build")).To(BeADirectory())
		Expect(files(f.store.CopyPath(runA, "go-build"))).To(BeEmpty())
		Expect(f.fake.CallsTo(driver.CreateSubvolume)).To(HaveLen(1))
		Expect(f.fake.CallsTo(driver.Snapshot)).To(BeEmpty())
		Expect(f.store.Dir(goBuild)).To(BeADirectory(), "the lineage exists from its first restore, so ls can show it")
	})

	It("An optional key invalidates a lineage when its files change", func(ctx context.Context) {
		By("given the latest generation recorded key hash H")
		checkout := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(checkout, "rust-toolchain.toml"), []byte("channel = \"1.80\"\n"), 0o644)).To(Succeed())
		hashH, err := cache.HashKey(checkout, []string{"rust-toolchain.toml"})
		Expect(err).NotTo(HaveOccurred())
		target := cache.Lineage{Scope: repoScope, Name: "target", KeyHash: hashH}
		_, err = f.store.Restore(ctx, runA, []cache.Lineage{target})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "target", "built-under-H")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted[0].Generation).To(Equal(1))
		manifest, err := f.store.Manifest(target)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Latest().KeyHash).To(Equal(hashH))

		By("when the checkout's rust-toolchain.toml hashes to H'")
		Expect(os.WriteFile(filepath.Join(checkout, "rust-toolchain.toml"), []byte("channel = \"1.81\"\n"), 0o644)).To(Succeed())
		lineages, err := cache.Lineages([]config.Cache{{Name: "target", Scope: config.Repo, Key: []string{"rust-toolchain.toml"}}}, repoScope, checkout)
		Expect(err).NotTo(HaveOccurred())
		Expect(lineages[0].KeyHash).NotTo(Equal(hashH))

		By("then the run restores target empty")
		restored, err := f.store.Restore(ctx, runB, lineages)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Generation).To(BeZero())
		Expect(restored[0].Reason).To(Equal("key changed"))
		Expect(files(f.store.CopyPath(runB, "target"))).To(BeEmpty())

		By("and the promoted generation records H'")
		Expect(f.store.Candidate(ctx, runB, "build")).To(Succeed())
		_, err = f.store.Promote(ctx, runB)
		Expect(err).NotTo(HaveOccurred())
		manifest, err = f.store.Manifest(target)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Latest().Number).To(Equal(2))
		Expect(manifest.Latest().KeyHash).To(Equal(lineages[0].KeyHash))
	})

	It("Promotion comes from the last evidence-closed step", func(ctx context.Context) {
		By("given step 1 closed on evidence after writing a")
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "a")
		Expect(f.store.Candidate(ctx, runA, "step-1")).To(Succeed())

		By("and step 2 wrote b and was cancelled")
		f.write(runA, "go-build", "b")
		// A cancelled step never reaches Candidate; nothing is taken.

		By("when the run finishes")
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())

		By("then the new generation contains a and not b")
		Expect(promoted).To(HaveLen(1))
		Expect(promoted[0].Generation).To(Equal(1))
		Expect(files(f.store.GenerationPath(goBuild, 1))).To(Equal([]string{"a"}))
		manifest, err := f.store.Manifest(goBuild)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Latest().Step).To(Equal("step-1"))
		Expect(manifest.Latest().Run).To(Equal(runA))
		Expect(manifest.Latest().CreatedAt).To(Equal(f.now))
		Expect(manifest.Latest().SizeBytes).To(Equal(uint64(2)), "du of the candidate: one two-byte file")
		Expect(f.fake.CallsTo(driver.DiskUsage)).To(HaveLen(1), "measured once, at promotion")
	})

	It("A later evidence-closed step replaces the candidate", func(ctx context.Context) {
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "a")
		Expect(f.store.Candidate(ctx, runA, "step-1")).To(Succeed())
		f.write(runA, "go-build", "b")
		Expect(f.store.Candidate(ctx, runA, "step-2")).To(Succeed())

		Expect(files(f.store.CandidatePath(runA, "go-build"))).To(ConsistOf("a", "b"))
		record, err := f.store.Record(runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(record.Candidate.Step).To(Equal("step-2"))
		Expect(f.fake.CallsTo(driver.SetReadOnly)).To(HaveLen(1), "step 1's snapshot was made writable to be deleted")
		Expect(f.fake.CallsTo(driver.DeleteSubvolume)).To(HaveLen(1))
		snapshots := f.fake.CallsTo(driver.Snapshot)
		Expect(snapshots).To(HaveLen(2))
		Expect(snapshots[1].Args[2]).To(BeTrue(), "the candidate is read-only")

		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(files(f.store.GenerationPath(goBuild, promoted[0].Generation))).To(ConsistOf("a", "b"))
	})

	It("A run with no evidence-closed step promotes nothing", func(ctx context.Context) {
		f.generation(goBuild, 1, "old")
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "b")

		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())

		Expect(promoted).To(BeEmpty())
		Expect(generations(f.store, goBuild)).To(Equal([]int{1}), "the generation count is unchanged")
	})

	It("A run that fails at review still promotes the build cache", func(ctx context.Context) {
		By("given the build step closed on evidence")
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "built")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())

		By("and the review step returned revise until the retry cap")
		f.write(runA, "go-build", "revised-1")
		f.write(runA, "go-build", "revised-2")

		By("when the run finishes")
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())

		By("then a new generation exists from the build step's snapshot")
		Expect(promoted).To(HaveLen(1))
		Expect(files(f.store.GenerationPath(goBuild, 1))).To(Equal([]string{"built"}))
		manifest, err := f.store.Manifest(goBuild)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Latest().Step).To(Equal("build"))
	})

	It("A lineage keeps its two newest generations", func(ctx context.Context) {
		By("given generations 1 and 2")
		f.generation(goBuild, 1, "one")
		f.generation(goBuild, 2, "two")

		By("when generation 3 is promoted")
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "three")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())

		By("then 2 and 3 exist and 1 is gone")
		Expect(promoted[0].Generation).To(Equal(3))
		Expect(promoted[0].Pruned).To(Equal([]int{1}))
		Expect(generations(f.store, goBuild)).To(Equal([]int{2, 3}))
		Expect(f.store.GenerationPath(goBuild, 1)).NotTo(BeAnExistingFile())
		Expect(files(f.store.GenerationPath(goBuild, 3))).To(ConsistOf("two", "three"))
		Expect(f.fake.CallsTo(driver.SetReadOnly)).To(ContainElement(driver.Call{
			Method: driver.SetReadOnly, Args: []any{f.store.GenerationPath(goBuild, 1), false},
		}), "the read-only generation is made writable before its owner deletes it")
		Expect(f.fake.CallsTo(driver.DeleteSubvolume)).To(ContainElement(driver.Call{
			Method: driver.DeleteSubvolume, Args: []any{f.store.GenerationPath(goBuild, 1), true},
		}), "the last deletion syncs the filesystem")
	})

	It("A global lineage over its cap is reset, not trimmed", func(ctx context.Context) {
		By("given a global lineage with a 1 GB cap and a run copy of 2 GB")
		f = newFixture(func(c *cache.Config) { c.GlobalCap = 1 << 30 })
		f.generation(goMod, 1, "m1")
		f.generation(goMod, 2, "m2")
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goMod})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-mod", "m3")
		Expect(f.store.Candidate(ctx, runA, "toolchain")).To(Succeed())
		f.fake.Script(driver.DiskUsage, uint64(2<<30))

		By("when the run finishes")
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())

		By("then no generation is promoted and the lineage has none")
		Expect(promoted).To(HaveLen(1))
		Expect(promoted[0].Reset).To(BeTrue())
		Expect(promoted[0].Generation).To(BeZero())
		Expect(promoted[0].Pruned).To(Equal([]int{1, 2}))
		Expect(generations(f.store, goMod)).To(BeEmpty())
		Expect(f.store.CandidatePath(runA, "go-mod")).NotTo(BeAnExistingFile())
		manifest, err := f.store.Manifest(goMod)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Reset).NotTo(BeNil())
		Expect(manifest.Reset.Reason).To(ContainSubstring("exceeds the global cap"))
		Expect(manifest.Reset.SizeBytes).To(Equal(uint64(2 << 30)))
		Expect(manifest.Reset.Cap).To(Equal(uint64(1 << 30)))

		By("and the next run restores it empty")
		restored, err := f.store.Restore(ctx, runB, []cache.Lineage{goMod})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Reason).To(Equal("no generation"))
	})

	It("A repo lineage over its cap is flagged, never refused", func(ctx context.Context) {
		f = newFixture(func(c *cache.Config) { c.RepoCap = 1 << 30 })
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "big")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())
		f.fake.Script(driver.DiskUsage, uint64(2<<30))

		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted[0].Generation).To(Equal(1))
		Expect(promoted[0].Reset).To(BeFalse())

		summaries, err := f.store.List()
		Expect(err).NotTo(HaveOccurred())
		Expect(summaries).To(HaveLen(1))
		Expect(summaries[0].OverCap).To(BeTrue())
	})

	It("Concurrent promotions to one lineage both succeed and the later wins", func(ctx context.Context) {
		By("given two runs of one repo restored generation 1")
		f.generation(goBuild, 1, "one")
		for _, run := range []string{runA, runB} {
			_, err := f.store.Restore(ctx, run, []cache.Lineage{goBuild})
			Expect(err).NotTo(HaveOccurred())
			f.write(run, "go-build", "by-"+run)
			Expect(f.store.Candidate(ctx, run, "build")).To(Succeed())
		}
		Expect(files(f.store.CopyPath(runA, "go-build"))).To(ConsistOf("one", "by-"+runA), "neither copy sees the other's write")
		Expect(files(f.store.CopyPath(runB, "go-build"))).To(ConsistOf("one", "by-"+runB))

		By("when both finish and promote")
		var wg sync.WaitGroup
		results := make([][]cache.Promoted, 2)
		errs := make([]error, 2)
		for i, run := range []string{runA, runB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				results[i], errs[i] = f.store.Promote(ctx, run)
			}()
		}
		wg.Wait()

		By("then the lineage has two new generations and the latest is from the run that promoted last")
		Expect(errs).To(Equal([]error{nil, nil}))
		Expect(generations(f.store, goBuild)).To(Equal([]int{2, 3}))
		numbers := []int{results[0][0].Generation, results[1][0].Generation}
		Expect(numbers).To(ConsistOf(2, 3))
		manifest, err := f.store.Manifest(goBuild)
		Expect(err).NotTo(HaveOccurred())
		last := runA
		if numbers[1] == 3 {
			last = runB
		}
		Expect(manifest.Latest().Run).To(Equal(last))
		Expect(files(f.store.GenerationPath(goBuild, 3))).To(ConsistOf("one", "by-"+last))
	})

	It("A promotion addressed outside the run's lineage root is refused", func(ctx context.Context) {
		By("given a run restored under an inner worker's root")
		inner := cache.New(f.fake, cache.Config{Root: filepath.Join(f.root, "inner"), Now: func() time.Time { return f.now }})
		_, err := inner.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		Expect(inner.Candidate(ctx, runA, "build")).To(Succeed())

		By("when the outer store is asked to promote from the inner's record")
		outer := cache.New(f.fake, cache.Config{Root: f.root, Now: func() time.Time { return f.now }})
		Expect(os.MkdirAll(outer.RunDir(runA), 0o755)).To(Succeed())
		record, err := inner.Record(runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(writeJSON(filepath.Join(outer.RunDir(runA), cache.RecordName), record)).To(Succeed())
		promoted, err := outer.Promote(ctx, runA)

		By("then it is refused and nothing is promoted")
		Expect(errors.Is(err, cache.ErrOutsideRoot)).To(BeTrue(), "%v", err)
		Expect(promoted).To(BeEmpty())
		Expect(outer.Dir(goBuild)).NotTo(BeAnExistingFile())
		Expect(f.fake.CallsTo(driver.Rename)).To(BeEmpty())
	})

	It("The manifest is the worker's, and the ledger records only use", func(ctx context.Context) {
		By("when a run restores and promotes lineage go-build")
		restored, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild})
		Expect(err).NotTo(HaveOccurred())
		f.write(runA, "go-build", "a")
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())
		f.now = f.now.Add(time.Minute)
		promoted, err := f.store.Promote(ctx, runA)
		Expect(err).NotTo(HaveOccurred())

		By("then the lineage's manifest.json records the generation's created-at, size estimate, producing run and step")
		var manifest cache.Manifest
		Expect(readJSON(filepath.Join(f.store.Dir(goBuild), cache.ManifestName), &manifest)).To(Succeed())
		Expect(manifest.Generations).To(HaveLen(1))
		Expect(manifest.Generations[0].CreatedAt).To(Equal(f.now))
		Expect(manifest.Generations[0].SizeBytes).To(Equal(uint64(2)))
		Expect(manifest.Generations[0].Run).To(Equal(runA))
		Expect(manifest.Generations[0].Step).To(Equal("build"))

		By("and what the ledger is handed says that the run restored and promoted go-build and nothing else about it")
		Expect(restored).To(Equal([]cache.Restored{{Lineage: goBuild, Reason: "no generation", RestoredAt: f.now.Add(-time.Minute)}}))
		Expect(promoted).To(Equal([]cache.Promoted{{Lineage: goBuild, Generation: 1, SizeBytes: 2}}))
	})

	It("Discard removes the run's copies, candidates and record", func(ctx context.Context) {
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild, goMod})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.store.Candidate(ctx, runA, "build")).To(Succeed())

		Expect(f.store.Discard(ctx, runA)).To(Succeed())

		Expect(filepath.Join(f.store.RunDir(runA), "cache")).NotTo(BeAnExistingFile())
		Expect(filepath.Join(f.store.RunDir(runA), "candidate")).NotTo(BeAnExistingFile())
		_, err = f.store.Record(runA)
		Expect(errors.Is(err, os.ErrNotExist)).To(BeTrue())
		Expect(f.fake.CallsTo(driver.DeleteSubvolume)).To(HaveLen(4), "two candidates and two copies")
		Expect(f.store.Discard(ctx, runA)).To(Succeed(), "and again is nothing")
	})

	It("refuses a lineage whose name or scope is not one path component", func(ctx context.Context) {
		for _, bad := range []cache.Lineage{
			{Scope: repoScope, Name: "../etc"},
			{Scope: repoScope, Name: ""},
			{Scope: "repo/../global", Name: "go-mod"},
			{Scope: "other", Name: "go-mod"},
		} {
			_, err := f.store.Restore(ctx, runA, []cache.Lineage{bad})
			Expect(err).To(HaveOccurred(), "%v", bad)
		}
		_, err := f.store.Restore(ctx, "../x", []cache.Lineage{goBuild})
		Expect(err).To(MatchError(ContainSubstring("run id")))
	})

	It("lists every lineage by scope with its generations, latest estimate, last restore and flags", func(ctx context.Context) {
		f.generation(goBuild, 1, "one")
		f.generation(goBuild, 2, "two")
		f.now = f.now.Add(time.Hour)
		_, err := f.store.Restore(ctx, runA, []cache.Lineage{goBuild, goMod})
		Expect(err).NotTo(HaveOccurred())

		summaries, err := f.store.List()
		Expect(err).NotTo(HaveOccurred())

		Expect(summaries).To(HaveLen(2))
		Expect(summaries[0].Lineage).To(Equal(goMod), "global first")
		Expect(summaries[0].Generations).To(BeEmpty())
		Expect(summaries[0].Latest).To(BeNil())
		Expect(summaries[1].Lineage).To(Equal(goBuild))
		Expect(summaries[1].Generations).To(Equal([]int{1, 2}))
		Expect(summaries[1].Latest.Number).To(Equal(2))
		Expect(summaries[1].LastRestoredAt).To(Equal(f.now))
		Expect(summaries[1].OverCap).To(BeFalse())
	})

	DescribeTable("RepoID is a readable, path-safe slug of the origin with a hash to keep origins apart",
		func(origin, prefix string) {
			id := cache.RepoID(origin)
			Expect(id).To(HavePrefix(prefix + "-"))
			Expect(id).To(MatchRegexp(`^[A-Za-z0-9][A-Za-z0-9._-]*-[0-9a-f]{8}$`))
		},
		Entry("https", "https://gitlab.com/ivy/mill.git", "gitlab.com-ivy-mill"),
		Entry("ssh", "git@gitlab.com:ivy/mill.git", "gitlab.com-ivy-mill"),
		Entry("a path", "/srv/git/mill", "srv-git-mill"),
	)

	It("RepoID keeps two origins with one slug apart", func() {
		Expect(cache.RepoID("https://gitlab.com/ivy/mill.git")).NotTo(Equal(cache.RepoID("git@gitlab.com:ivy/mill.git")))
		Expect(cache.RepoID("https://gitlab.com/ivy/mill.git")).To(Equal(cache.RepoID("https://gitlab.com/ivy/mill.git")))
	})
})
