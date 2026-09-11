package cache_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The live tier: lineages on the sandbox's btrfs volume through the real
// systemd driver, as the worker's own uid and never through sudo — every
// subvolume here is created, snapshotted and deleted by the account running
// the spec (docs/spikes/nspawn-per-run-sandbox.md E9). Off unless
// MILL_SANDBOX_HOST says so (docs/sandbox.md §Testing).
//
// Nothing here touches /var/lib/mill/cache or /var/lib/mill/runs: the store
// is rooted in a directory of the spec's own under the volume, swept with
// the suite.

const (
	liveVar = "MILL_SANDBOX_HOST"
	// hostRoot is the sandbox root on a host; inside a machine the root is
	// the nested worker's (docs/sandbox.md §Self-hosting), see liveRoot.
	hostRoot = "/var/lib/mill"
	// liveFill is the generation the timed scenario restores and promotes:
	// four GiB, as the criterion says.
	liveFill = 4 << 30
	// liveBudget is what restore and promote must each finish within.
	liveBudget = time.Second
)

// liveRoot is the sandbox root this spec process works under: the host's
// volume, or — inside a machine, which is where this suite runs once mill
// builds mill — the nested worker's subvolume under the ephemeral root,
// made here as the worker's own uid the way `mill run` would. Off the
// ephemeral root would be a bind of the outer's, which is what a nested run
// must never promote into.
func liveRoot(ctx context.Context) string {
	GinkgoHelper()
	host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
	if manager, err := host.Container(ctx); err != nil || manager != driver.ContainerNspawn {
		return hostRoot
	}
	Expect(driver.EnsureNestedRoot(ctx, host, driver.NestedRoot)).To(Succeed())
	Expect(driver.CheckEphemeralRoot(driver.NestedRoot)).To(Succeed())
	return driver.NestedRoot
}

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

// isSubvolume reports whether a path is the root of a btrfs subvolume: its
// inode number is 256, which `btrfs subvolume show` would say too if it
// did not need root.
func isSubvolume(path string) bool {
	GinkgoHelper()
	return strings.TrimSpace(must("stat", "-c", "%i", path)) == "256"
}

// readOnly is the subvolume's ro property.
func readOnly(path string) bool {
	GinkgoHelper()
	return strings.TrimSpace(must("btrfs", "property", "get", "-ts", path, "ro")) == "ro=true"
}

// sweep deletes every subvolume under a directory, deepest first, then the
// directory. A read-only one is made writable first.
func sweep(root string) {
	var subvolumes []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if out, _, status := host("stat", "-c", "%i", path); status == 0 && strings.TrimSpace(out) == "256" && path != root {
			subvolumes = append(subvolumes, path)
		}
		return nil
	})
	for i := len(subvolumes) - 1; i >= 0; i-- {
		host("btrfs", "property", "set", "-ts", subvolumes[i], "ro", "false")
		host("btrfs", "subvolume", "delete", subvolumes[i])
	}
	host("btrfs", "filesystem", "sync", root)
	_ = os.RemoveAll(root)
}

// fill writes about size bytes of random data into dir as many files under
// a fan of directories, the shape of a build cache.
func fill(dir string, size int64) {
	GinkgoHelper()
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var written int64
	for i := 0; written < size; i++ {
		sub := filepath.Join(dir, fmt.Sprintf("%02x", i%256))
		Expect(os.MkdirAll(sub, 0o755)).To(Succeed())
		_, err := rand.Read(buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(sub, fmt.Sprintf("%06d", i)), buf, 0o644)).To(Succeed())
		written += chunk
	}
	must("sync", dir)
}

// seed makes the lineage's generations 1..n by running one promoting run
// per generation, each writing a marker, and returns the store.
func seed(ctx context.Context, store *cache.Store, lineage cache.Lineage, markers ...string) {
	GinkgoHelper()
	for i, marker := range markers {
		run := fmt.Sprintf("SEED%s%d", lineage.Name, i+1)
		_, err := store.Restore(ctx, run, []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(store.CopyPath(run, lineage.Name), marker), []byte(marker), 0o644)).To(Succeed())
		Expect(store.Candidate(ctx, run, "seed")).To(Succeed())
		promoted, err := store.Promote(ctx, run)
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted[0].Generation).To(Equal(i + 1))
		Expect(store.Discard(ctx, run)).To(Succeed())
	}
}

var _ = Describe("Cache lineages on the sandbox volume", Label("live"), Serial, Ordered, func() {
	var (
		store *cache.Store
		root  string
		repo  = cache.Repo("live-" + cache.RepoID("https://example.invalid/live/repo.git"))
	)

	BeforeAll(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that snapshots real lineages")
		}
		sandbox := liveRoot(ctx)
		var err error
		root, err = os.MkdirTemp(sandbox, "cache-live-*")
		Expect(err).NotTo(HaveOccurred(), "the sandbox root %s is not this account's", sandbox)
		DeferCleanup(func() { sweep(root) })
		host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
		fs, err := host.Statfs(root)
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.Btrfs()).To(BeTrue(), "%s is %s, not btrfs", root, fs.Type)
		store = cache.New(host, cache.Config{Root: root, GlobalCap: 1 << 30})
	})

	It("The latest generation restores and the run's writes do not touch it", func(ctx context.Context) {
		By("given lineage go-build with generation 2 containing marker")
		lineage := cache.Lineage{Scope: repo, Name: "go-build"}
		seed(ctx, store, lineage, "older", "marker")
		Expect(readOnly(store.GenerationPath(lineage, 2))).To(BeTrue())

		By("when a run restores it and the session writes to the cache path")
		restored, err := store.Restore(ctx, "01LIVEA", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Generation).To(Equal(2))
		copyPath := store.CopyPath("01LIVEA", "go-build")
		Expect(os.WriteFile(filepath.Join(copyPath, "written"), []byte("by the run"), 0o644)).To(Succeed())

		By("then marker is present in the run's copy")
		Expect(filepath.Join(copyPath, "marker")).To(BeARegularFile())
		Expect(isSubvolume(copyPath)).To(BeTrue(), "the copy is a subvolume of its own")
		Expect(readOnly(copyPath)).To(BeFalse(), "and writable")

		By("and generation 2 is unchanged")
		Expect(filepath.Join(store.GenerationPath(lineage, 2), "written")).NotTo(BeAnExistingFile())
		Expect(files(store.GenerationPath(lineage, 2))).To(ConsistOf("older", "marker"))
		err = os.WriteFile(filepath.Join(store.GenerationPath(lineage, 2), "direct"), []byte("x"), 0o644)
		Expect(err).To(HaveOccurred(), "a generation is read-only even to its owner")

		By("and an absent lineage restores an empty writable subvolume")
		fresh := cache.Lineage{Scope: repo, Name: "fresh"}
		restored, err = store.Restore(ctx, "01LIVEB", []cache.Lineage{fresh})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Generation).To(BeZero())
		Expect(isSubvolume(store.CopyPath("01LIVEB", "fresh"))).To(BeTrue())
		Expect(readOnly(store.CopyPath("01LIVEB", "fresh"))).To(BeFalse())
		Expect(files(store.CopyPath("01LIVEB", "fresh"))).To(BeEmpty())
		Expect(store.Discard(ctx, "01LIVEA")).To(Succeed())
		Expect(store.Discard(ctx, "01LIVEB")).To(Succeed())
	})

	It("Restore time does not scale with cache size", func(ctx context.Context) {
		By("given a lineage whose latest generation is four gigabytes")
		lineage := cache.Lineage{Scope: repo, Name: "big"}
		_, err := store.Restore(ctx, "01FILL", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		fill(store.CopyPath("01FILL", "big"), liveFill)
		Expect(store.Candidate(ctx, "01FILL", "fill")).To(Succeed())
		promoted, err := store.Promote(ctx, "01FILL")
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted[0].SizeBytes).To(BeNumerically(">=", liveFill), "du -s of the generation")
		Expect(store.Discard(ctx, "01FILL")).To(Succeed())

		By("when a run restores it, and promotes it after a write")
		start := time.Now()
		restored, err := store.Restore(ctx, "01TIMED", []cache.Lineage{lineage})
		restoreTook := time.Since(start)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Generation).To(Equal(1))
		Expect(os.WriteFile(filepath.Join(store.CopyPath("01TIMED", "big"), "timed"), []byte("x"), 0o644)).To(Succeed())
		start = time.Now()
		Expect(store.Candidate(ctx, "01TIMED", "build")).To(Succeed())
		promoted, err = store.Promote(ctx, "01TIMED")
		promoteTook := time.Since(start)
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("restore %s, candidate+promote %s for %d bytes\n", restoreTook, promoteTook, promoted[0].SizeBytes)

		By("then each completes in under one second")
		Expect(restoreTook).To(BeNumerically("<", liveBudget))
		Expect(promoteTook).To(BeNumerically("<", liveBudget))
		Expect(promoted[0].Generation).To(Equal(2))
		Expect(filepath.Join(store.GenerationPath(lineage, 2), "timed")).To(BeARegularFile())
		Expect(store.Discard(ctx, "01TIMED")).To(Succeed())
	})

	It("Promotion comes from the last evidence-closed step", func(ctx context.Context) {
		By("given step 1 closed on evidence after writing a")
		lineage := cache.Lineage{Scope: repo, Name: "steps"}
		_, err := store.Restore(ctx, "01STEPS", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		copyPath := store.CopyPath("01STEPS", "steps")
		Expect(os.WriteFile(filepath.Join(copyPath, "a"), []byte("a"), 0o644)).To(Succeed())
		Expect(store.Candidate(ctx, "01STEPS", "step-1")).To(Succeed())
		Expect(readOnly(store.CandidatePath("01STEPS", "steps"))).To(BeTrue())

		By("and step 2 wrote b and was cancelled")
		Expect(os.WriteFile(filepath.Join(copyPath, "b"), []byte("b"), 0o644)).To(Succeed())

		By("when the run finishes")
		promoted, err := store.Promote(ctx, "01STEPS")
		Expect(err).NotTo(HaveOccurred())

		By("then the new generation contains a and not b")
		Expect(promoted[0].Generation).To(Equal(1))
		Expect(files(store.GenerationPath(lineage, 1))).To(Equal([]string{"a"}))
		Expect(readOnly(store.GenerationPath(lineage, 1))).To(BeTrue())
		Expect(store.CandidatePath("01STEPS", "steps")).NotTo(BeAnExistingFile(), "the candidate became the generation")
		Expect(store.Discard(ctx, "01STEPS")).To(Succeed())
	})

	It("A lineage keeps its two newest generations", func(ctx context.Context) {
		By("given generations 1 and 2")
		lineage := cache.Lineage{Scope: repo, Name: "kept"}
		seed(ctx, store, lineage, "one", "two")

		By("when generation 3 is promoted")
		_, err := store.Restore(ctx, "01THREE", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(store.CopyPath("01THREE", "kept"), "three"), []byte("3"), 0o644)).To(Succeed())
		Expect(store.Candidate(ctx, "01THREE", "build")).To(Succeed())
		promoted, err := store.Promote(ctx, "01THREE")
		Expect(err).NotTo(HaveOccurred())

		By("then 2 and 3 exist and 1 is gone")
		Expect(promoted[0].Generation).To(Equal(3))
		Expect(promoted[0].Pruned).To(Equal([]int{1}))
		Expect(generations(store, lineage)).To(Equal([]int{2, 3}))
		Expect(store.GenerationPath(lineage, 1)).NotTo(BeAnExistingFile())
		Expect(files(store.GenerationPath(lineage, 3))).To(ConsistOf("one", "two", "three"))
		Expect(store.Discard(ctx, "01THREE")).To(Succeed())
	})

	It("A global lineage over its cap is reset, not trimmed", func(ctx context.Context) {
		By("given a global lineage with a 1 GB cap and a run copy of 2 GB")
		lineage := cache.Lineage{Scope: cache.Global, Name: "live-cargo-registry"}
		seed(ctx, store, lineage, "small")
		_, err := store.Restore(ctx, "01OVER", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		fill(store.CopyPath("01OVER", "live-cargo-registry"), 2<<30)
		Expect(store.Candidate(ctx, "01OVER", "toolchain")).To(Succeed())

		By("when the run finishes")
		promoted, err := store.Promote(ctx, "01OVER")
		Expect(err).NotTo(HaveOccurred())

		By("then no generation is promoted and the lineage has none")
		Expect(promoted[0].Reset).To(BeTrue())
		Expect(promoted[0].SizeBytes).To(BeNumerically(">", 1<<30))
		Expect(generations(store, lineage)).To(BeEmpty())
		Expect(store.GenerationPath(lineage, 1)).NotTo(BeAnExistingFile())
		Expect(store.CandidatePath("01OVER", "live-cargo-registry")).NotTo(BeAnExistingFile())
		manifest, err := store.Manifest(lineage)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Reset.Reason).To(ContainSubstring("exceeds the global cap"))
		Expect(store.Discard(ctx, "01OVER")).To(Succeed())

		By("and the next run restores it empty")
		restored, err := store.Restore(ctx, "01AFTER", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored[0].Reason).To(Equal("no generation"))
		Expect(store.Discard(ctx, "01AFTER")).To(Succeed())
	})

	It("Concurrent promotions to one lineage both succeed and the later wins", func(ctx context.Context) {
		By("given two runs of one repo restored generation 1")
		lineage := cache.Lineage{Scope: repo, Name: "shared"}
		seed(ctx, store, lineage, "one")
		runs := []string{"01CONCA", "01CONCB"}
		for _, run := range runs {
			_, err := store.Restore(ctx, run, []cache.Lineage{lineage})
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(store.CopyPath(run, "shared"), "by-"+run), []byte(run), 0o644)).To(Succeed())
			Expect(store.Candidate(ctx, run, "build")).To(Succeed())
		}

		By("when both finish and promote")
		var wg sync.WaitGroup
		results := make([][]cache.Promoted, 2)
		errs := make([]error, 2)
		for i, run := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				results[i], errs[i] = store.Promote(ctx, run)
			}()
		}
		wg.Wait()

		By("then the lineage has two new generations and the latest is from the run that promoted last")
		Expect(errs).To(Equal([]error{nil, nil}))
		Expect(generations(store, lineage)).To(Equal([]int{2, 3}))
		manifest, err := store.Manifest(lineage)
		Expect(err).NotTo(HaveOccurred())
		latest := manifest.Latest()
		Expect(latest.Number).To(Equal(3))
		Expect(files(store.GenerationPath(lineage, 3))).To(ConsistOf("one", "by-"+latest.Run))

		By("and neither run's copy was affected by the other")
		for _, run := range runs {
			Expect(files(store.CopyPath(run, "shared"))).To(ConsistOf("one", "by-"+run))
			Expect(store.Discard(ctx, run)).To(Succeed())
		}
	})
})

// The nested half of docs/sandbox.md §Caching's "a nested run never promotes
// into the outer's lineages": run inside a machine by the machine package's
// live tier, this is the inner run — a step closed on evidence, a promotion
// — against the nested worker's own root. On a host it runs the same code
// against the host's volume and sweeps itself; inside a machine it leaves
// its generation standing, under the ephemeral root, for the outer's spec
// to find, and the machine's end is its sweep.
var _ = Describe("A nested run's lineages", Label("live"), Serial, func() {
	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that snapshots real lineages")
		}
	})

	// Scenario: A nested run does not promote into the outer's lineages
	//   Given an inner run whose step closed on evidence
	//   When the inner run finishes
	//   Then the outer repo's lineage has no new generation
	It("A nested run promotes under its own root and nowhere else", func(ctx context.Context) {
		host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
		sandbox := liveRoot(ctx)
		inside := sandbox != hostRoot
		root := filepath.Join(sandbox, "cache-live-nested")
		sweep(root)
		Expect(os.MkdirAll(root, 0o755)).To(Succeed())
		if !inside {
			DeferCleanup(func() { sweep(root) })
		}
		store := cache.New(host, cache.Config{Root: root, GlobalCap: 1 << 30})
		lineage := cache.Lineage{Scope: cache.Repo("nested-" + cache.RepoID("https://example.invalid/nested/repo.git")), Name: "go-build"}

		By("given an inner run whose step closed on evidence")
		_, err := store.Restore(ctx, "01NESTED", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(store.CopyPath("01NESTED", "go-build"), "inner"), []byte("inner"), 0o644)).To(Succeed())
		Expect(store.Candidate(ctx, "01NESTED", "step-1")).To(Succeed())

		By("when the inner run finishes")
		promoted, err := store.Promote(ctx, "01NESTED")
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Discard(ctx, "01NESTED")).To(Succeed())

		By("then its generation is under its own root")
		Expect(promoted).To(HaveLen(1))
		generation := store.GenerationPath(lineage, promoted[0].Generation)
		Expect(generation).To(HavePrefix(root + "/"))
		Expect(isSubvolume(generation)).To(BeTrue())
		Expect(files(generation)).To(Equal([]string{"inner"}))
		if inside {
			Expect(generation).To(HavePrefix(driver.NestedRoot+"/"), "inside a machine the root is the nested worker's")
			point, err := driver.MountPoint(generation)
			Expect(err).NotTo(HaveOccurred())
			Expect(point).To(Equal("/"), "on the ephemeral root, not on a bind of the outer's")
		}

		By("and a promotion addressed outside the worker's root is refused")
		other := cache.New(host, cache.Config{Root: filepath.Join(sandbox, "cache-live-other"), GlobalCap: 1 << 30})
		_, err = other.Restore(ctx, "01ELSEWHERE", []cache.Lineage{lineage})
		Expect(err).NotTo(HaveOccurred())
		Expect(other.Candidate(ctx, "01ELSEWHERE", "step-1")).To(Succeed())
		Expect(os.Rename(other.RunDir("01ELSEWHERE"), store.RunDir("01ELSEWHERE"))).To(Succeed())
		_, err = store.Promote(ctx, "01ELSEWHERE")
		Expect(err).To(MatchError(cache.ErrOutsideRoot))
		Expect(os.Rename(store.RunDir("01ELSEWHERE"), other.RunDir("01ELSEWHERE"))).To(Succeed())
		Expect(other.Discard(ctx, "01ELSEWHERE")).To(Succeed())
		sweep(other.Config().Root)
	})
})
