package cli

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Where the configuration file is read from when nothing names one: the
// account's own file first, and the file the package installs under /etc
// when the account has none (docs/tools.md §Configuration).
var _ = Describe("The default configuration path", Label("unit"), func() {
	var user, system string

	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		user = filepath.Join(dir, "home", "mill", "config.yaml")
		system = filepath.Join(dir, "etc", "mill", "config.yaml")
	})

	write := func(path string) {
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte("workspace: local\n"), 0o644)).To(Succeed())
	}

	It("is the account's file when only it exists", func() {
		write(user)
		Expect(configPath(user, system)).To(Equal(user))
	})

	It("is the system's file when the account has none", func() {
		write(system)
		Expect(configPath(user, system)).To(Equal(system))
	})

	It("is the account's file when both exist", func() {
		write(user)
		write(system)
		Expect(configPath(user, system)).To(Equal(user))
	})

	It("is the account's path when neither exists, so a missing file is not an error", func() {
		Expect(configPath(user, system)).To(Equal(user))
	})

	It("is the account's path when the account has no home and no system file exists", func() {
		Expect(configPath("", system)).To(BeEmpty())
	})
})
