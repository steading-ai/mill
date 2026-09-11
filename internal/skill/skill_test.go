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
