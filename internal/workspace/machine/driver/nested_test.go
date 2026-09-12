package driver_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The inner worker's root (docs/sandbox.md §Self-hosting): a subvolume made
// once, however many workers inside the same machine ask for it at once.
var _ = Describe("The nested root", Label("unit"), func() {
	var (
		host *driver.FakeDriver
		root string
	)

	BeforeEach(func() {
		host = driver.NewFake()
		root = filepath.Join(GinkgoT().TempDir(), "mill")
	})

	It("is created when absent and left alone when present", func(ctx context.Context) {
		Expect(driver.EnsureNestedRoot(ctx, host, root)).To(Succeed())
		Expect(root).To(BeADirectory())
		Expect(driver.EnsureNestedRoot(ctx, host, root)).To(Succeed())
		Expect(host.CallsTo(driver.CreateSubvolume)).To(HaveLen(1), "a root that is there is not created again")
	})

	It("takes a create refused because another worker's create landed first as that worker's root", func(ctx context.Context) {
		// The loser of the race: the root was absent when it looked, and
		// by the time its create is refused the winner's subvolume is there.
		Expect(driver.EnsureNestedRoot(ctx, racedCreate{host}, root)).To(Succeed())
		Expect(root).To(BeADirectory())
		Expect(host.CallsTo(driver.CreateSubvolume)).To(HaveLen(1))
	})

	It("refuses a create that fails with nothing there, and a root that is not a directory", func(ctx context.Context) {
		refused := errors.New("btrfs subvolume create: exec: \"btrfs\": executable file not found in $PATH")
		host.Script(driver.CreateSubvolume, refused)
		Expect(driver.EnsureNestedRoot(ctx, host, root)).To(MatchError(refused))

		Expect(os.WriteFile(root, []byte("x"), 0o600)).To(Succeed())
		Expect(driver.EnsureNestedRoot(ctx, host, root)).To(MatchError(ContainSubstring("not a directory")))
	})
})

// racedCreate is a host on which another worker's create lands during this
// one's: the subvolume appears, and this create is refused for it.
type racedCreate struct{ *driver.FakeDriver }

func (r racedCreate) CreateSubvolume(ctx context.Context, path string) error {
	if err := r.FakeDriver.CreateSubvolume(ctx, path); err != nil {
		return err
	}
	return errors.New("btrfs subvolume create " + path + " exited 1: ERROR: Could not create subvolume: File exists")
}
