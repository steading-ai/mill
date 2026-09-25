package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/workspace/machine/cache"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
	"github.com/steading-ai/mill/internal/workspace/machine/image"
)

// prune deletes with `btrfs subvolume delete`, which a plain directory
// cannot stand in for, so the removal and its printing are proven through
// the same function the command runs, over the fake driver.
var _ = Describe("mill cache prune", Label("unit"), func() {
	It("prints each lineage and image it removed, the pass and the reason, then free space", func(ctx context.Context) {
		now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		fake := driver.NewFake()
		root := GinkgoT().TempDir()
		store := cache.New(fake, cache.Config{Root: root, Now: func() time.Time { return now }})
		images := image.New(fake, image.Config{Root: root, Machines: GinkgoT().TempDir(), Now: func() time.Time { return now }})
		base := image.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

		By("given a lineage restored 40 days ago, an image booted 35 days ago, and fresh ones beside them")
		stale := cache.Lineage{Scope: cache.Repo("gitlab.com-ivy-old-0a1b2c3d"), Name: "go-build"}
		fresh := cache.Lineage{Scope: cache.Repo("gitlab.com-ivy-mill-0a1b2c3d"), Name: "go-build"}
		now = now.Add(-40 * 24 * time.Hour)
		seedLineage(ctx, store, "01OLD", stale)
		oldImage, _, err := images.Ensure(ctx, base, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		now = now.Add(5 * 24 * time.Hour)
		Expect(images.Booted(oldImage)).To(Succeed())
		now = now.Add(35 * 24 * time.Hour)
		seedLineage(ctx, store, "01NEW", fresh)
		freshImage, _, err := images.Ensure(ctx, base, []string{"vips-devel"})
		Expect(err).NotTo(HaveOccurred())
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 50 << 30})

		By("when prune runs")
		var stdout bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetOut(&stdout)
		Expect(runCachePrune(cmd, store, store.Policy(images))).To(Succeed())

		By("then it names what it removed and why")
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		Expect(lines).To(HaveLen(4), stdout.String())
		Expect(strings.Fields(lines[0])).To(Equal([]string{"KIND", "NAME", "LAST", "USED", "PASS", "REASON"}))
		Expect(strings.Fields(lines[1])).To(Equal([]string{"lineage", stale.String(), "gen-1", "2026-08-02T12:00:00Z", "expiry",
			"unused", "for", "40d,", "past", "the", "expiry", "of", "30d"}))
		Expect(strings.Fields(lines[2])).To(Equal([]string{"image", oldImage.Short(), "2026-08-07T12:00:00Z", "expiry",
			"unused", "for", "35d,", "past", "the", "expiry", "of", "30d"}))
		Expect(lines[3]).To(Equal("free 50G of 100G (high watermark 10G, low 20G)"))
		Expect(stdout.String()).NotTo(ContainSubstring(fresh.String()))
		Expect(stdout.String()).NotTo(ContainSubstring(freshImage.Short()))
		Expect(store.GenerationPath(stale, 1)).NotTo(BeAnExistingFile())
		Expect(store.GenerationPath(fresh, 1)).To(BeADirectory())
		Expect(images.Path(oldImage)).NotTo(BeAnExistingFile())
		Expect(images.Path(freshImage)).To(BeADirectory())
	})

	It("says so when there is nothing to evict", func(ctx context.Context) {
		fake := driver.NewFake()
		store := cache.New(fake, cache.Config{Root: GinkgoT().TempDir()})
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 50 << 30})
		var stdout bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetOut(&stdout)
		Expect(runCachePrune(cmd, store, store.Policy(nil))).To(Succeed())
		Expect(stdout.String()).To(Equal("nothing to evict under " + store.LineageRoot() + "\nfree 50G of 100G (high watermark 10G, low 20G)\n"))
	})
})

// seedLineage makes gen-1 of a lineage by one promoting run.
func seedLineage(ctx context.Context, store *cache.Store, run string, lineage cache.Lineage) {
	GinkgoHelper()
	_, err := store.Restore(ctx, run, []cache.Lineage{lineage})
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(store.CopyPath(run, lineage.Name), "f"), []byte("x"), 0o644)).To(Succeed())
	Expect(store.Candidate(ctx, run, "build")).To(Succeed())
	_, err = store.Promote(ctx, run)
	Expect(err).NotTo(HaveOccurred())
	Expect(store.Discard(ctx, run)).To(Succeed())
}
