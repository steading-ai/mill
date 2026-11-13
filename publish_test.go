package mill_test

import (
	"os"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

// The publish workflow (.github/workflows/publish.yml) is a push, not a
// check. These specs read it as
// data rather than running it — nothing here can reach ghcr.io — so what
// they prove is the shape a reviewer would otherwise have to eyeball: the
// trigger, the permissions, and the tags a push actually pushes.
var _ = Describe("The publish workflow", Label("unit"), func() {
	const path = ".github/workflows/publish.yml"

	var (
		text string
		doc  map[string]interface{}
	)

	BeforeEach(func() {
		body, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		text = string(body)
		Expect(yaml.Unmarshal(body, &doc)).To(Succeed())
	})

	It("runs on push to main and nothing else — no pull request", func() {
		on, ok := doc["on"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "the workflow has an `on` block")
		Expect(on).To(HaveLen(1), "the workflow has exactly one trigger")
		Expect(on).To(HaveKey("push"))
		Expect(on).NotTo(HaveKey("pull_request"))

		push, ok := on["push"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "`on.push` is a mapping")
		branches, ok := push["branches"].([]interface{})
		Expect(ok).To(BeTrue(), "`on.push.branches` is a list")
		Expect(branches).To(ConsistOf("main"))
	})

	It("authenticates with GITHUB_TOKEN alone, under packages: write", func() {
		Expect(doc["permissions"]).To(Equal(map[string]interface{}{
			"contents": "read",
			"packages": "write",
		}))

		secret := regexp.MustCompile(`secrets\.[A-Z_]+`)
		seen := map[string]bool{}
		for _, match := range secret.FindAllString(text, -1) {
			seen[match] = true
		}
		Expect(seen).To(Equal(map[string]bool{"secrets.GITHUB_TOKEN": true}))
	})

	It("pushes the full commit SHA and main, for the image packaging/Containerfile builds", func() {
		Expect(text).To(ContainSubstring("ghcr.io/steading-ai/mill:${{ github.sha }}"))
		Expect(text).To(ContainSubstring("ghcr.io/steading-ai/mill:main"))
		Expect(text).To(ContainSubstring("--file packaging/Containerfile"))
	})

	It("stages the same tree make package builds from, for each architecture", func() {
		Expect(text).To(MatchRegexp(`make stage-"\$arch"`))
		Expect(text).To(ContainSubstring("amd64"))
		Expect(text).To(ContainSubstring("arm64"))
	})
})
