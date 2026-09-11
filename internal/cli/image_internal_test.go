package cli

import (
	"bytes"
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// prune deletes with `btrfs subvolume delete`, which a plain directory
// cannot stand in for, so the removal is proven through the same function
// the command runs, over the fake driver whose delete removes the
// directory.
var _ = Describe("mill image prune", Label("unit"), func() {
	It("removes an image whose last boot is older than the expiry", func(ctx context.Context) {
		now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		fake := driver.NewFake()
		store := image.New(fake, image.Config{Root: GinkgoT().TempDir(), Machines: GinkgoT().TempDir(), Now: func() time.Time { return now }})
		base := image.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

		old, _, err := store.Ensure(ctx, base, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Booted(old)).To(Succeed())
		now = now.Add(31 * 24 * time.Hour)
		fresh, _, err := store.Ensure(ctx, base, []string{"vips-devel"})
		Expect(err).NotTo(HaveOccurred())

		var stdout bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetOut(&stdout)
		Expect(runImagePrune(cmd, store, image.DefaultExpiry, false)).To(Succeed())

		Expect(stdout.String()).To(ContainSubstring(old.Short()))
		Expect(stdout.String()).To(ContainSubstring("removed"))
		Expect(stdout.String()).NotTo(ContainSubstring(fresh.Short()))
		Expect(fake.CallsTo(driver.DeleteSubvolume)).To(Equal([]driver.Call{
			{Method: driver.DeleteSubvolume, Args: []any{store.Path(old), false}},
		}))
		Expect(store.Path(old)).NotTo(BeAnExistingFile())
		Expect(store.Path(fresh)).To(BeADirectory())
	})
})
