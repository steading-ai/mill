package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The configuration file's keys are documented per command in docs/tools.md
// §Configuration. The table there is hand-kept, and these specs are what
// keep it: the list the table is rendered from is checked against the
// Config struct and against applyConfig's bindings, and the rendering
// against the document.

var _ = Describe("The config-key table", Label("unit"), func() {
	// fileKeys is every key the file may carry, as the Config struct
	// spells it.
	fileKeys := func() []string {
		var keys []string
		t := reflect.TypeFor[Config]()
		for i := range t.NumField() {
			keys = append(keys, t.Field(i).Tag.Get("yaml"))
		}
		return keys
	}

	It("lists every key the file may carry, and nothing else", func() {
		listed := slices.Clone(unreadKeys)
		for _, key := range configKeys {
			listed = append(listed, key.key)
		}
		Expect(listed).To(ConsistOf(fileKeys()))
	})

	It("names, for every string setting applyConfig binds, the flag it fills", func() {
		flagged := map[string]bool{}
		for _, key := range configKeys {
			if key.flag != "" {
				flagged[key.flag] = true
			}
		}
		opts := workOptions{}
		for flag := range stringTargets(&opts) {
			Expect(flagged).To(HaveKey(flag), "a bound flag the table does not know")
		}
		for flag := range stringValues(Config{}) {
			Expect(flagged).To(HaveKey(flag), "a read value the table does not know")
		}
	})

	It("declares every flagged key on work and on serve, so the file reaches both", func() {
		root := NewRootCommand(io.Discard, io.Discard)
		for _, name := range []string{"work", "serve"} {
			cmd, _, err := root.Find([]string{name})
			Expect(err).NotTo(HaveOccurred())
			for _, key := range configKeys {
				Expect(honoursKey(cmd, key)).To(BeTrue(), "%s does not honour %s", name, key.key)
			}
		}
	})

	It("matches the declared flags: docs/tools.md carries the table the commands render", func() {
		root := NewRootCommand(io.Discard, io.Discard)
		rendered := configKeyTable(root)
		body, err := os.ReadFile(filepath.Join("..", "..", "docs", "tools.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Contains(string(body), rendered)).To(BeTrue(),
			"docs/tools.md §Configuration does not carry this table:\n%s", rendered)
	})
})
