package mill_test

import (
	"encoding/json"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// iscText is the ISC license as https://opensource.org/license/isc gives
// it, with the copyright holder filled in.
const iscText = `Copyright 2026 Ivy Evans <ivy@ivyevans.net>

Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted, provided that the above copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.`

// pluginManifest is the part of a Claude Code plugin or marketplace
// manifest these specs read.
type pluginManifest struct {
	Version string `json:"version"`
	License string `json:"license"`
	Plugins []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"plugins"`
}

func readManifest(path string) pluginManifest {
	GinkgoHelper()
	raw, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	var m pluginManifest
	Expect(json.Unmarshal(raw, &m)).To(Succeed(), path)
	return m
}

var _ = Describe("The licence", Label("unit"), func() {
	It("is the ISC license, word for word, under an ISC License heading", func() {
		raw, err := os.ReadFile("LICENSE.md")
		Expect(err).NotTo(HaveOccurred())

		heading, body, found := strings.Cut(string(raw), "\n\n")
		Expect(found).To(BeTrue(), "LICENSE.md is a heading, a blank line, then the text")
		Expect(heading).To(Equal("# ISC License"))
		Expect(strings.TrimRight(body, " \t\r\n")).To(Equal(iscText))
	})

	It("is named and linked from the README", func() {
		raw, err := os.ReadFile("README.md")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("[ISC license](LICENSE.md)"))
	})

	It("is the one the package declares", func() {
		raw, err := os.ReadFile("packaging/nfpm.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(MatchRegexp(`(?m)^license: ISC$`))
	})

	It("is the one the plugin declares, at one version in both manifests", func() {
		plugin := readManifest("plugin/mill/.claude-plugin/plugin.json")
		marketplace := readManifest("plugin/.claude-plugin/marketplace.json")

		Expect(plugin.License).To(Equal("ISC"))
		Expect(plugin.Version).NotTo(BeEmpty())
		Expect(marketplace.Plugins).To(HaveLen(1))
		Expect(marketplace.Plugins[0].Version).To(Equal(plugin.Version),
			"the marketplace entry and the plugin manifest are bumped together")
	})
})
