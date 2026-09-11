package image_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// baseA is a base digest as Harbor would spell it, less the prefix.
const baseA = image.Digest("a3f1c2d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")

// dnfNoMatch is what dnf says of a package that is not there, with
// podman's own closing line after it (docs/spikes/nspawn-per-run-sandbox.md
// E10).
const dnfNoMatch = "STEP 2/2: RUN dnf install -y libfooo-devel && dnf clean all\n" +
	"Updating and loading repositories:\n" +
	"Repositories loaded.\n" +
	"Failed to resolve the transaction:\n" +
	"No match for argument: libfooo-devel\n" +
	"You can try to add to command line:\n" +
	"  --skip-unavailable to skip unavailable packages\n" +
	"Error: building at STEP \"RUN dnf install -y libfooo-devel && dnf clean all\": while running runtime: exit status 1\n"

// fixture is a store over the fake driver with a temporary root and
// machines directory, and a clock the spec advances.
type fixture struct {
	fake     *machine.FakeDriver
	store    *image.Store
	root     string
	machines string
	now      time.Time
}

func newFixture(pin string) *fixture {
	GinkgoHelper()
	f := &fixture{
		fake:     machine.NewFake(),
		root:     GinkgoT().TempDir(),
		machines: GinkgoT().TempDir(),
		now:      time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
	}
	f.store = image.New(f.fake, image.Config{
		Root:     f.root,
		Machines: f.machines,
		Registry: "harbor.example",
		Pin:      pin,
		Now:      func() time.Time { return f.now },
	})
	return f
}

// subvolume makes a directory stand in for a built image, the way the fake
// driver's CreateSubvolume does.
func (f *fixture) subvolume(digest image.Digest) {
	GinkgoHelper()
	Expect(os.Mkdir(f.store.Path(digest), 0o755)).To(Succeed())
}

var _ = Describe("The derived digest", Label("unit"), func() {
	It("The derived digest is a function of base and packages", func() {
		By("given base digest A")
		By("when two mill.tomls declare the same packages in different orders")
		first := image.Derived(baseA, []string{"vips-devel", "libpq-devel"})
		second := image.Derived(baseA, []string{"libpq-devel", "vips-devel", "libpq-devel"})

		By("then both derive the same digest")
		Expect(first).To(Equal(second))
		Expect(string(first)).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(first).NotTo(Equal(baseA))
	})

	It("is the base itself when no packages are declared", func() {
		Expect(image.Derived(baseA, nil)).To(Equal(baseA))
		Expect(image.Derived(baseA, []string{"", " "})).To(Equal(baseA))
	})

	It("differs when the packages or the base differ", func() {
		one := image.Derived(baseA, []string{"libpq-devel"})
		Expect(image.Derived(baseA, []string{"vips-devel"})).NotTo(Equal(one))
		other := image.Digest(strings.Repeat("b", 64))
		Expect(image.Derived(other, []string{"libpq-devel"})).NotTo(Equal(one))
	})

	It("reads a digest with or without the sha256 prefix and refuses anything else", func() {
		digest, err := image.ParseDigest("sha256:" + string(baseA))
		Expect(err).NotTo(HaveOccurred())
		Expect(digest).To(Equal(baseA))
		Expect(digest.OCI()).To(Equal("sha256:" + string(baseA)))
		_, err = image.ParseDigest("latest")
		Expect(err).To(MatchError(ContainSubstring("not a sha256 digest")))
		_, err = image.ParseDigest("sha256:" + strings.ToUpper(string(baseA)))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Ensuring an image subvolume", Label("unit"), func() {
	var f *fixture

	BeforeEach(func() { f = newFixture("") })

	It("A missing derived image is built once", func(ctx context.Context) {
		By("given no subvolume for the digest")
		packages := []string{"libpq-devel"}
		digest := image.Derived(baseA, packages)
		Expect(f.store.Path(digest)).NotTo(BeAnExistingFile())

		By("when two runs of the repo are provisioned at once")
		var wg sync.WaitGroup
		results := make([]image.Digest, 2)
		findings := make([]*image.Finding, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				results[i], findings[i], errs[i] = f.store.Ensure(ctx, baseA, packages)
			}()
		}
		wg.Wait()

		By("then the second waits on the lock and then finds the subvolume")
		Expect(errs).To(Equal([]error{nil, nil}))
		Expect(findings).To(Equal([]*image.Finding{nil, nil}))
		Expect(results).To(Equal([]image.Digest{digest, digest}))
		Expect(f.fake.CallsTo(machine.BuildImageOnHost)).To(HaveLen(1), "one build for two callers")
		Expect(f.fake.CallsTo(machine.Exists)).To(HaveLen(2), "each caller checked under the lock")
		Expect(f.store.Path(digest)).To(BeADirectory())
		Expect(f.store.Path(digest) + image.BuildingSuffix).NotTo(BeAnExistingFile())
	})

	It("builds from a Containerfile pinning the base by digest and installing the packages, and exports with the capabilities restored", func(ctx context.Context) {
		digest, finding, err := f.store.Ensure(ctx, baseA, []string{"vips-devel", "libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(finding).To(BeNil())

		build := f.fake.CallsTo(machine.BuildImageOnHost)[0].Args[0].(machine.ImageBuild)
		Expect(build.Containerfile).NotTo(BeAnExistingFile(), "the build context is temporary and gone")
		Expect(build.Tags).To(ConsistOf("localhost/mill/derived:" + string(digest)))
		Expect(f.store.Containerfile(baseA, []string{"libpq-devel", "vips-devel"})).To(Equal(
			"FROM harbor.example/mill/base@sha256:" + string(baseA) + "\n" +
				"RUN dnf install -y libpq-devel vips-devel && dnf clean all\n"))
		Expect(f.store.Containerfile(baseA, nil)).To(Equal("FROM harbor.example/mill/base@sha256:" + string(baseA) + "\n"))

		building := f.store.Path(digest) + image.BuildingSuffix
		Expect(f.fake.CallsTo(machine.CreateSubvolume)[0].Args).To(Equal([]any{building}))
		Expect(f.fake.CallsTo(machine.ExportImage)[0].Args).To(Equal([]any{build.Tags[0], building}))
		Expect(f.fake.CallsTo(machine.Setcap)).To(Equal([]machine.Call{
			{Method: machine.Setcap, Args: []any{"cap_setuid=ep", filepath.Join(building, "usr/bin/newuidmap")}},
			{Method: machine.Setcap, Args: []any{"cap_setgid=ep", filepath.Join(building, "usr/bin/newgidmap")}},
		}))
		Expect(f.fake.CallsTo(machine.Rename)[0].Args).To(Equal([]any{building, f.store.Path(digest)}))
		Expect(f.fake.CallsTo(machine.RemoveImage)[0].Args).To(Equal([]any{build.Tags[0]}), "the built image is not kept in container storage")

		manifest, err := f.store.Manifest(digest)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest).To(Equal(image.Manifest{Digest: digest, Base: baseA, Packages: []string{"libpq-devel", "vips-devel"}, BuiltAt: f.now}))
	})

	It("uses a subvolume that is already there without building", func(ctx context.Context) {
		digest := image.Derived(baseA, []string{"libpq-devel"})
		f.subvolume(digest)

		got, finding, err := f.store.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(finding).To(BeNil())
		Expect(got).To(Equal(digest))
		Expect(f.fake.CallsTo(machine.BuildImageOnHost)).To(BeEmpty())
	})

	It("A package that does not exist degrades to base with a finding", func(ctx context.Context) {
		By("given mill.toml declares libfooo-devel")
		f.fake.Script(machine.BuildImageOnHost, machine.Output{Stdout: dnfNoMatch, Status: 1})

		By("when the image is ensured")
		digest, finding, err := f.store.Ensure(ctx, baseA, []string{"libfooo-devel"})
		Expect(err).NotTo(HaveOccurred())

		By("then the run boots from base")
		Expect(digest).To(Equal(baseA))
		Expect(f.store.Path(baseA)).To(BeADirectory(), "the base subvolume was ensured for the fallback")
		Expect(f.fake.CallsTo(machine.BuildImageOnHost)).To(HaveLen(2), "the derived build, then the base's export")
		Expect(f.fake.CallsTo(machine.CreateSubvolume)).To(HaveLen(1), "no .building was made for the failed build")
		Expect(f.store.Path(image.Derived(baseA, []string{"libfooo-devel"})) + image.BuildingSuffix).NotTo(BeAnExistingFile())

		By("and the first session receives a finding naming libfooo-devel and dnf's error")
		Expect(finding).NotTo(BeNil())
		Expect(finding.Packages).To(Equal([]string{"libfooo-devel"}))
		Expect(finding.Stage).To(Equal("build"))
		Expect(finding.Output).To(ContainSubstring("No match for argument: libfooo-devel"))
		Expect(finding.Output).NotTo(ContainSubstring("Error: building at STEP"), "podman's closing line is not dnf's")
		Expect(finding.String()).To(ContainSubstring("libfooo-devel"))
		Expect(finding.String()).To(ContainSubstring("boots from base " + baseA.Short()))
	})

	It("removes .building and degrades to base when the export fails after the build", func(ctx context.Context) {
		f.fake.Script(machine.ExportImage, errors.New("podman export exited 125: no space left"))

		digest, finding, err := f.store.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(digest).To(Equal(baseA))
		Expect(finding.Stage).To(Equal("export"))
		Expect(finding.Err).To(MatchError(ContainSubstring("no space left")))

		derived := image.Derived(baseA, []string{"libpq-devel"})
		Expect(f.fake.CallsTo(machine.DeleteSubvolume)[0].Args).To(Equal([]any{f.store.Path(derived) + image.BuildingSuffix}))
		Expect(f.store.Path(derived) + image.BuildingSuffix).NotTo(BeAnExistingFile())
		Expect(f.store.Path(derived)).NotTo(BeAnExistingFile())
		Expect(f.fake.CallsTo(machine.RemoveImage)).To(HaveLen(2), "the built image is removed on failure too")
	})

	It("carries only the last twenty lines of the build in the finding", func(ctx context.Context) {
		var lines []string
		for i := range 30 {
			lines = append(lines, "line "+string(rune('a'+i)))
		}
		f.fake.Script(machine.BuildImageOnHost, machine.Output{Stdout: strings.Join(lines, "\n") + "\n", Status: 1})

		_, finding, err := f.store.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Split(finding.Output, "\n")).To(HaveLen(image.TailLines))
		Expect(finding.Output).To(HavePrefix("line k"))
	})

	It("fails, rather than degrading, when the base itself cannot be exported", func(ctx context.Context) {
		f.fake.Script(machine.BuildImageOnHost, errors.New("podman build exited 125: manifest unknown"))

		_, finding, err := f.store.Ensure(ctx, baseA, nil)
		Expect(err).To(MatchError(image.ErrNoBase))
		Expect(err).To(MatchError(ContainSubstring("manifest unknown")))
		Expect(finding).NotTo(BeNil())
	})

	It("gives up on the derived build at the budget and boots base", func(ctx context.Context) {
		slow := image.New(f.fake, image.Config{Root: f.root, Machines: f.machines, BuildBudget: time.Nanosecond})
		f.fake.Script(machine.BuildImageOnHost, context.DeadlineExceeded)

		digest, finding, err := slow.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(digest).To(Equal(baseA))
		Expect(finding.Err).To(MatchError(ContainSubstring("budget")))
	})
})

var _ = Describe("Resolving the base", Label("unit"), func() {
	var f *fixture

	BeforeEach(func() { f = newFixture("") })

	It("pulls the latest tag from Harbor, reads its digest, and remembers it", func(ctx context.Context) {
		f.fake.Script(machine.ImageDigest, "sha256:"+string(baseA))

		base, err := f.store.ResolveBase(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(base).To(Equal(image.Base{Digest: baseA, Reference: "harbor.example/mill/base:latest"}))
		Expect(f.fake.CallsTo(machine.PullImage)[0].Args).To(Equal([]any{"harbor.example/mill/base:latest"}))
		Expect(filepath.Join(f.root, "images", "base.json")).To(BeAnExistingFile())
	})

	It("resolves a pinned tag from Harbor and a pinned digest from nowhere", func(ctx context.Context) {
		tagged := newFixture("2026-09-01")
		tagged.fake.Script(machine.ImageDigest, "sha256:"+string(baseA))
		base, err := tagged.store.ResolveBase(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(base.Reference).To(Equal("harbor.example/mill/base:2026-09-01"))
		Expect(base.Pinned).To(BeFalse())

		pinned := newFixture("sha256:" + string(baseA))
		base, err = pinned.store.ResolveBase(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(base).To(Equal(image.Base{Digest: baseA, Reference: "harbor.example/mill/base@sha256:" + string(baseA), Pinned: true}))
		Expect(pinned.fake.Calls()).To(BeEmpty(), "a pinned digest asks nothing of the host")
	})

	It("falls back to the last resolved base when Harbor is unreachable and its subvolume exists", func(ctx context.Context) {
		By("given a base resolved earlier whose subvolume is on the host")
		f.fake.Script(machine.ImageDigest, "sha256:"+string(baseA))
		_, err := f.store.ResolveBase(ctx)
		Expect(err).NotTo(HaveOccurred())
		f.subvolume(baseA)

		By("when Harbor does not answer")
		f.fake.Script(machine.PullImage, errors.New("podman pull exited 125: dial tcp: no route to host"))
		base, err := f.store.ResolveBase(ctx)

		By("then the provision uses it and records the fallback")
		Expect(err).NotTo(HaveOccurred())
		Expect(base.Digest).To(Equal(baseA))
		Expect(base.Fallback).To(BeTrue())
		Expect(base.Reason).To(ContainSubstring("no route to host"))
		Expect(base.Reason).To(ContainSubstring("harbor.example/mill/base:latest"))
	})

	It("fails before any machine starts when Harbor is unreachable and no resolved base has a subvolume", func(ctx context.Context) {
		f.fake.Script(machine.PullImage, errors.New("podman pull exited 125: dial tcp: no route to host"))
		_, err := f.store.ResolveBase(ctx)
		Expect(err).To(MatchError(image.ErrNoBase))
		Expect(err).To(MatchError(ContainSubstring("no base was resolved before")))

		By("and likewise when the base was resolved but its subvolume is gone")
		f.fake.Script(machine.ImageDigest, "sha256:"+string(baseA))
		_, err = f.store.ResolveBase(ctx)
		Expect(err).NotTo(HaveOccurred())
		f.fake.Script(machine.PullImage, errors.New("podman pull exited 125: dial tcp: no route to host"))
		_, err = f.store.ResolveBase(ctx)
		Expect(err).To(MatchError(image.ErrNoBase))
		Expect(err).To(MatchError(ContainSubstring("has no subvolume")))
		Expect(f.fake.CallsTo(machine.StartUnit)).To(BeEmpty())
	})
})

var _ = Describe("Listing and pruning images", Label("unit"), func() {
	var f *fixture

	BeforeEach(func() { f = newFixture("") })

	It("lists a built subvolume with its base and packages, and records boots", func(ctx context.Context) {
		digest, _, err := f.store.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		_, _, err = f.store.Ensure(ctx, baseA, nil)
		Expect(err).NotTo(HaveOccurred())

		f.now = f.now.Add(time.Hour)
		Expect(f.store.Booted(digest)).To(Succeed())

		listed, err := f.store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(2))
		Expect(listed[0].Digest).To(Equal(baseA), "least recently used first")
		Expect(listed[1]).To(Equal(image.Manifest{
			Digest: digest, Base: baseA, Packages: []string{"libpq-devel"},
			BuiltAt: f.now.Add(-time.Hour), LastBootedAt: f.now,
		}))
	})

	It("does not list a record whose subvolume is gone", func(ctx context.Context) {
		digest, _, err := f.store.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.RemoveAll(f.store.Path(digest))).To(Succeed())

		listed, err := f.store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(BeEmpty())
	})

	It("prunes an image whose last boot is older than the expiry and keeps the rest", func(ctx context.Context) {
		old, _, err := f.store.Ensure(ctx, baseA, []string{"libpq-devel"})
		Expect(err).NotTo(HaveOccurred())
		f.now = f.now.Add(24 * time.Hour)
		fresh, _, err := f.store.Ensure(ctx, baseA, []string{"vips-devel"})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.store.Booted(old)).To(Succeed())
		f.now = f.now.Add(31 * 24 * time.Hour)
		Expect(f.store.Booted(fresh)).To(Succeed())

		pruned, err := f.store.Prune(ctx, image.DefaultExpiry)
		Expect(err).NotTo(HaveOccurred())
		Expect(pruned).To(HaveLen(1))
		Expect(pruned[0].Digest).To(Equal(old))
		Expect(f.fake.CallsTo(machine.DeleteSubvolume)[0].Args).To(Equal([]any{f.store.Path(old)}))
		Expect(f.store.Path(old)).NotTo(BeAnExistingFile())
		Expect(f.store.Path(fresh)).To(BeADirectory())

		listed, err := f.store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(1))
		Expect(listed[0].Digest).To(Equal(fresh))
	})
})

var _ = Describe("Building the base", Label("unit"), func() {
	var f *fixture

	BeforeEach(func() { f = newFixture("") })

	It("builds the context with the Harbor host as a build argument, tags the date and latest, and pushes when asked", func(ctx context.Context) {
		f.fake.Script(machine.ImageDigest, "sha256:"+string(baseA))
		robot := filepath.Join(GinkgoT().TempDir(), "harbor-robot")
		Expect(os.WriteFile(robot, []byte("robot$mill+builder:s3cret\n"), 0o600)).To(Succeed())
		credential, err := image.LoadRobot(robot)
		Expect(err).NotTo(HaveOccurred())

		result, err := f.store.BuildBase(ctx, image.BaseBuild{Context: "images/base", Push: true, Robot: credential})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(image.BaseResult{
			Tags:   []string{"harbor.example/mill/base:2026-09-11", "harbor.example/mill/base:latest"},
			Digest: baseA,
			Pushed: true,
		}))
		Expect(f.fake.CallsTo(machine.BuildImageOnHost)[0].Args[0]).To(Equal(machine.ImageBuild{
			Context: "images/base",
			Tags:    result.Tags,
			Args:    map[string]string{"HARBOR_HOST": "harbor.example"},
		}))
		Expect(f.fake.CallsTo(machine.Login)[0].Args).To(Equal([]any{"harbor.example", "robot$mill+builder"}))
		Expect(f.fake.CallsTo(machine.PushImage)).To(HaveLen(2))
		Expect(f.fake.CallsTo(machine.Logout)).To(HaveLen(1))
	})

	It("builds without pushing by default and refuses a push with no credential", func(ctx context.Context) {
		f.fake.Script(machine.ImageDigest, "sha256:"+string(baseA), "sha256:"+string(baseA))
		result, err := f.store.BuildBase(ctx, image.BaseBuild{Context: "images/base"})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Pushed).To(BeFalse())
		Expect(f.fake.CallsTo(machine.PushImage)).To(BeEmpty())

		_, err = f.store.BuildBase(ctx, image.BaseBuild{Context: "images/base", Push: true})
		Expect(err).To(MatchError(ContainSubstring("robot credential")))
	})

	It("reads the robot credential as one line of robot:secret", func() {
		_, err := image.LoadRobot("")
		Expect(err).To(MatchError(ContainSubstring("CREDENTIALS_DIRECTORY")))
		bad := filepath.Join(GinkgoT().TempDir(), "harbor-robot")
		Expect(os.WriteFile(bad, []byte("no-colon\n"), 0o600)).To(Succeed())
		_, err = image.LoadRobot(bad)
		Expect(err).To(MatchError(ContainSubstring("<robot>:<secret>")))
	})
})
