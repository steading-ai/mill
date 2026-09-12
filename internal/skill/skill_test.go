package skill_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/skill"
)

// The skill a machine mounts is what the worker wrote from its own embed,
// so the specs hold the materialised directory to the plugin's tree and to
// the version the worker was told.

var _ = Describe("The materialised skill", Label("unit"), func() {
	var dir string

	BeforeEach(func() {
		dir = filepath.Join(GinkgoT().TempDir(), "skill")
	})

	It("is the plugin's skill with the worker's version beside it", func() {
		By("when the worker materialises the skill it embeds")
		Expect(skill.Materialise(dir, "1.2.3")).To(Succeed())

		By("then SKILL.md and its references are the plugin's, byte for byte")
		for _, name := range []string{"SKILL.md", "references/tools.md"} {
			shipped, err := os.ReadFile(filepath.Join("..", "..", "plugin", "mill", "skills", "mill", name))
			Expect(err).NotTo(HaveOccurred())
			written, err := os.ReadFile(filepath.Join(dir, name))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(written)).To(Equal(string(shipped)), name)
		}

		By("and the breadcrumb is the version, one line")
		body, err := os.ReadFile(filepath.Join(dir, skill.VersionFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("1.2.3\n"))
		Expect(skill.Version(dir)).To(Equal("1.2.3"))
	})

	It("replaces what an earlier worker left, and nothing else survives", func() {
		By("given a directory holding a placeholder skill and an older breadcrumb")
		Expect(os.MkdirAll(filepath.Join(dir, "stale"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# placeholder\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "stale", "old.md"), []byte("gone\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, skill.VersionFile), []byte("0.9.0\n"), 0o644)).To(Succeed())

		By("when the worker materialises")
		Expect(skill.Materialise(dir, "1.0.0")).To(Succeed())

		By("then the placeholder is the skill, the breadcrumb is current, and the stale entry is gone")
		body, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(HavePrefix("---\nname: mill\n"))
		Expect(skill.Version(dir)).To(Equal("1.0.0"))
		Expect(filepath.Join(dir, "stale")).NotTo(BeAnExistingFile())
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		Expect(names).To(ConsistOf("SKILL.md", "references", skill.VersionFile))
	})

	It("leaves an unchanged file's inode alone, so a bound machine sees nothing move", func() {
		Expect(skill.Materialise(dir, "1.0.0")).To(Succeed())
		before, err := os.Stat(filepath.Join(dir, "SKILL.md"))
		Expect(err).NotTo(HaveOccurred())

		Expect(skill.Materialise(dir, "1.0.0")).To(Succeed())
		after, err := os.Stat(filepath.Join(dir, "SKILL.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(os.SameFile(before, after)).To(BeTrue())
	})

	It("refuses a version that is not one line", func() {
		Expect(skill.Materialise(dir, "1.0.0\nextra")).To(MatchError(ContainSubstring("not one line")))
		Expect(skill.Materialise(dir, "")).To(MatchError(ContainSubstring("not one line")))
	})
})

// The sidecar the worker rides into a machine is copied from where it was
// built, not embedded, so the specs hold the copy to the source and to the
// version gate: refreshed when the binary's version differs, left alone when
// it does not.
var _ = Describe("The materialised sidecar", Label("unit"), func() {
	var source, dest string

	// build stands in for the built sidecar tree: a dist entrypoint and a
	// scrap of node_modules, which is all the copy has to carry faithfully.
	build := func(mainBody string) {
		GinkgoHelper()
		Expect(os.MkdirAll(filepath.Join(source, "dist"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(source, "dist", "main.js"), []byte(mainBody), 0o644)).To(Succeed())
		// pnpm's layout: the package lives under `.pnpm` and `node_modules/pkg`
		// is a relative link to it; `.bin` carries an executable shim.
		Expect(os.MkdirAll(filepath.Join(source, "node_modules", ".pnpm", "pkg@1", "node_modules", "pkg"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(source, "node_modules", ".pnpm", "pkg@1", "node_modules", "pkg", "index.js"), []byte("mod\n"), 0o644)).To(Succeed())
		if _, err := os.Lstat(filepath.Join(source, "node_modules", "pkg")); err != nil {
			Expect(os.Symlink(filepath.Join(".pnpm", "pkg@1", "node_modules", "pkg"), filepath.Join(source, "node_modules", "pkg"))).To(Succeed())
		}
		Expect(os.MkdirAll(filepath.Join(source, "node_modules", ".bin"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(source, "node_modules", ".bin", "shim"), []byte("#!/bin/sh\n"), 0o755)).To(Succeed())
		// What a checkout keeps beside the build and a machine has no use for.
		Expect(os.MkdirAll(filepath.Join(source, "src"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(source, "src", "main.ts"), []byte("ts\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(source, "sidecar_test.go"), []byte("go\n"), 0o644)).To(Succeed())
	}

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		source = filepath.Join(tmp, "src", "sidecar")
		dest = filepath.Join(tmp, "share", "sidecar")
	})

	It("copies the built sidecar and writes the version beside it", func() {
		build("v1\n")
		Expect(skill.MaterialiseSidecar(source, dest, "1.2.3")).To(Succeed())

		body, err := os.ReadFile(filepath.Join(dest, "dist", "main.js"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("v1\n"))
		mod, err := os.ReadFile(filepath.Join(dest, "node_modules", "pkg", "index.js"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(mod)).To(Equal("mod\n"))
		Expect(skill.Version(dest)).To(Equal("1.2.3"))

		By("and pnpm's relative links are links in the copy, resolving inside it")
		pointer, err := os.Readlink(filepath.Join(dest, "node_modules", "pkg"))
		Expect(err).NotTo(HaveOccurred())
		Expect(pointer).To(Equal(filepath.Join(".pnpm", "pkg@1", "node_modules", "pkg")))

		By("and a shim keeps its executable bit")
		info, err := os.Stat(filepath.Join(dest, "node_modules", ".bin", "shim"))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o755)))

		By("and carries only the built tree: dist, node_modules and the breadcrumb")
		entries, err := os.ReadDir(dest)
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		Expect(names).To(ConsistOf("dist", "node_modules", skill.VersionFile))
	})

	// Scenario: The sidecar rides in read-only (the materialise half: the
	// tree under the share root matches the binary's version).
	It("is refreshed when the binary's version differs and left alone when it does not", func() {
		build("v1\n")
		Expect(skill.MaterialiseSidecar(source, dest, "1.0.0")).To(Succeed())

		By("a newer build under the same version is not copied: the gate is the breadcrumb")
		build("v2\n")
		Expect(skill.MaterialiseSidecar(source, dest, "1.0.0")).To(Succeed())
		body, err := os.ReadFile(filepath.Join(dest, "dist", "main.js"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("v1\n"), "unchanged version leaves the copy alone")

		By("a differing version refreshes the whole tree")
		Expect(skill.MaterialiseSidecar(source, dest, "2.0.0")).To(Succeed())
		body, err = os.ReadFile(filepath.Join(dest, "dist", "main.js"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("v2\n"))
		Expect(skill.Version(dest)).To(Equal("2.0.0"))
	})

	It("prunes what a newer version's build no longer carries", func() {
		build("v1\n")
		Expect(skill.MaterialiseSidecar(source, dest, "1.0.0")).To(Succeed())
		Expect(os.RemoveAll(filepath.Join(source, "node_modules", ".bin"))).To(Succeed())

		Expect(skill.MaterialiseSidecar(source, dest, "1.0.1")).To(Succeed())
		Expect(filepath.Join(dest, "node_modules", ".bin")).NotTo(BeADirectory())
	})

	It("refuses a link that leaves the sidecar: a machine has nothing at its far end", func() {
		build("v1\n")
		Expect(os.Symlink(filepath.Join("..", "..", "..", "elsewhere"), filepath.Join(source, "node_modules", "away"))).To(Succeed())

		Expect(skill.MaterialiseSidecar(source, dest, "1.0.0")).To(MatchError(ContainSubstring("points outside the sidecar")))
	})

	It("refuses a source that is not a directory or not a built sidecar, and a version that is not one line", func() {
		Expect(skill.MaterialiseSidecar(filepath.Join(source, "absent"), dest, "1.0.0")).
			To(MatchError(ContainSubstring("not a directory")))

		By("a directory with no dist/main.js is not a sidecar, and nothing is written for it")
		Expect(os.MkdirAll(filepath.Join(source, "src"), 0o755)).To(Succeed())
		Expect(skill.MaterialiseSidecar(source, dest, "1.0.0")).
			To(MatchError(ContainSubstring("not a built sidecar")))
		Expect(dest).NotTo(BeADirectory())

		build("v1\n")
		Expect(skill.MaterialiseSidecar(source, dest, "1.0.0\nextra")).To(MatchError(ContainSubstring("not one line")))
	})
})

// The workflow directory a check step's scripts come from rides into the
// machine as a copy: links resolved, modes kept, nothing else left behind.
var _ = Describe("The materialised workflow directory", Label("unit"), func() {
	var source, dest string

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		// The shape the acceptance suite stages: a definition of its own
		// beside links into the tree it derives from.
		tree := filepath.Join(tmp, "tree", "quick-fix")
		Expect(os.MkdirAll(filepath.Join(tree, "scripts"), 0o755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(tree, "skills", "commit"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tree, "scripts", "clean-tree"), []byte("#!/bin/sh\nexit 0\n"), 0o755)).To(Succeed()) //nolint:gosec // a script
		Expect(os.WriteFile(filepath.Join(tree, "skills", "commit", "SKILL.md"), []byte("# commit\n"), 0o644)).To(Succeed())
		source = filepath.Join(tmp, "staged", "derived")
		Expect(os.MkdirAll(source, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(source, "workflow.yaml"), []byte("name: derived\n"), 0o644)).To(Succeed())
		Expect(os.Symlink(filepath.Join(tree, "scripts"), filepath.Join(source, "scripts"))).To(Succeed())
		Expect(os.Symlink(filepath.Join(tree, "skills"), filepath.Join(source, "skills"))).To(Succeed())
		dest = filepath.Join(tmp, "share", "workflow")
	})

	It("copies the directory with its links resolved and its scripts still executable", func() {
		Expect(skill.MaterialiseWorkflow(source, dest)).To(Succeed())

		script, err := os.Lstat(filepath.Join(dest, "scripts", "clean-tree"))
		Expect(err).NotTo(HaveOccurred())
		Expect(script.Mode().IsRegular()).To(BeTrue(), "a link is copied as the file it names")
		Expect(script.Mode().Perm()&0o111).NotTo(BeZero(), "the script's mode rides")
		Expect(filepath.Join(dest, "skills", "commit", "SKILL.md")).To(BeAnExistingFile())
		Expect(filepath.Join(dest, "workflow.yaml")).To(BeAnExistingFile())
	})

	It("prunes what an earlier directory had that this one does not, and leaves an unchanged file's inode alone", func() {
		Expect(skill.MaterialiseWorkflow(source, dest)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dest, "stale"), []byte("x"), 0o644)).To(Succeed())
		before, err := os.Stat(filepath.Join(dest, "scripts", "clean-tree"))
		Expect(err).NotTo(HaveOccurred())

		Expect(skill.MaterialiseWorkflow(source, dest)).To(Succeed())

		Expect(filepath.Join(dest, "stale")).NotTo(BeAnExistingFile())
		after, err := os.Stat(filepath.Join(dest, "scripts", "clean-tree"))
		Expect(err).NotTo(HaveOccurred())
		Expect(os.SameFile(before, after)).To(BeTrue())
	})

	It("refuses a source that is not a directory", func() {
		Expect(skill.MaterialiseWorkflow(filepath.Join(source, "workflow.yaml"), dest)).To(MatchError(ContainSubstring("not a directory")))
		Expect(dest).NotTo(BeADirectory())
	})
})
