package machine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The eviction triggers (docs/sandbox.md §Eviction): the watermark check
// after a promotion, and both passes at worker start through
// machine.Evict, over the same fake host the workspace runs on.
var _ = Describe("Eviction triggers", Label("unit"), func() {
	var (
		f     *fixture
		spec  workspace.Spec
		log   bytes.Buffer
		other cache.Lineage
	)

	// seedOther makes gen-1 of another repository's lineage, last used
	// `ago` before now.
	seedOther := func(ctx context.Context, ago time.Duration) {
		GinkgoHelper()
		then := time.Now().Add(-ago)
		store := cache.New(f.host, cache.Config{Root: f.root, Now: func() time.Time { return then }})
		_, err := store.Restore(ctx, "01OTHER", []cache.Lineage{other})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(store.CopyPath("01OTHER", "go-build"), "object"), []byte("built\n"), 0o600)).To(Succeed())
		Expect(store.Candidate(ctx, "01OTHER", "build")).To(Succeed())
		_, err = store.Promote(ctx, "01OTHER")
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Discard(ctx, "01OTHER")).To(Succeed())
	}

	BeforeEach(func() {
		f = newFixture(origin(map[string]string{"mise.toml": "[tools]\ngo = \"1.25\"\n"}))
		log.Reset()
		f.config.Log = &log
		f.config.Caches = cache.Config{HighWatermark: cache.Watermark{Percent: 10}, LowWatermark: cache.Watermark{Percent: 20}}
		spec = workspace.Spec{RunID: runID, NodeRef: "mill#67", Origin: f.config.Origin}
		other = cache.Lineage{Scope: cache.Repo("gitlab.com-ivy-other-0a1b2c3d"), Name: "go-build"}
	})

	// Scenario: Eviction runs at each trigger
	//   Then eviction runs after each promotion
	//   And at worker start after reconciliation
	It("Eviction runs at each trigger", func(ctx context.Context) {
		By("given another repository's lineage used ten days ago, and a run whose step closed on evidence")
		seedOther(ctx, 10*24*time.Hour)
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(f.runDir(), "cache", "go-build", "object"), []byte("built\n"), 0o600)).To(Succeed())
		Expect(ws.Candidate(ctx, "test")).To(Succeed())
		mine := cache.Lineage{Scope: cache.Repo(cache.RepoID(f.config.Origin)), Name: "go-build"}
		caches := cache.New(f.host, cache.Config{Root: f.root})

		By("when the run is destroyed with free space below the high watermark")
		f.host.Script(driver.Statfs,
			driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 5 << 30},
			driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 25 << 30})
		Expect(ws.Destroy(ctx)).To(Succeed())

		By("then eviction ran after the promotion: the least recently used repo lineage went, the run's own and the globals stayed")
		Expect(f.host.CallsTo(driver.Statfs)).To(HaveLen(2), "free space read, then re-read after the one deletion")
		Expect(caches.GenerationPath(other, 1)).NotTo(BeAnExistingFile())
		Expect(caches.GenerationPath(mine, 1)).To(BeADirectory())
		Expect(caches.GenerationPath(cache.Lineage{Scope: cache.Global, Name: "go-mod"}, 1)).To(BeADirectory())
		Expect(log.String()).To(ContainSubstring("evicted lineage " + other.String()))
		Expect(log.String()).To(ContainSubstring("below the high watermark 10%"))

		By("and at worker start, both passes run: an expired lineage goes whatever the free space")
		seedOther(ctx, 31*24*time.Hour)
		f.host.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 50 << 30})
		report, err := machine.Evict(ctx, f.host, nil, f.config)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Deleted).To(HaveLen(1))
		Expect(report.Deleted[0].Pass).To(Equal(cache.PassExpiry))
		Expect(report.Deleted[0].Lineage).To(Equal(other))
		Expect(caches.GenerationPath(other, 1)).NotTo(BeAnExistingFile())
		Expect(caches.GenerationPath(mine, 1)).To(BeADirectory())
	})

	It("A run that promotes nothing triggers no eviction", func(ctx context.Context) {
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Destroy(ctx)).To(Succeed())
		Expect(f.host.CallsTo(driver.Statfs)).To(BeEmpty())
	})
})
