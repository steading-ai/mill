package cli

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The rate table a worker prices usage with: the config file's own `rates:`
// key when it names one, else the packaged default (docs/ledger.md §usage).
var _ = Describe("loadRates", Label("unit"), func() {
	It("returns the configured table unchanged and never reads the packaged path", func() {
		configured := map[string]RateConfig{"sonnet": {Input: 3, Output: 15}}
		rates, err := loadRates(configured, filepath.Join(GinkgoT().TempDir(), "absent.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(rates).To(Equal(configured))
	})

	It("falls back to the packaged path when the config names none", func() {
		path := filepath.Join(GinkgoT().TempDir(), "rates.yaml")
		Expect(os.WriteFile(path, []byte(`
sonnet:
  input: 3
  output: 15
  cache_read: 0.3
  cache_write: 3.75
`), 0o600)).To(Succeed())

		rates, err := loadRates(nil, path)
		Expect(err).NotTo(HaveOccurred())
		Expect(rates).To(Equal(map[string]RateConfig{
			"sonnet": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		}))
	})

	It("is an empty table, not an error, when the packaged path does not exist", func() {
		rates, err := loadRates(nil, filepath.Join(GinkgoT().TempDir(), "absent.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(rates).To(BeEmpty())
	})

	It("refuses a packaged file it cannot parse", func() {
		path := filepath.Join(GinkgoT().TempDir(), "rates.yaml")
		Expect(os.WriteFile(path, []byte("sonnet: [unclosed\n"), 0o600)).To(Succeed())
		_, err := loadRates(nil, path)
		Expect(err).To(HaveOccurred())
	})

	It("parses the packaged default this repo ships", func() {
		rates, err := loadRates(nil, filepath.Join("..", "..", "packaging", "rates.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(rates).NotTo(BeEmpty())
	})
})
