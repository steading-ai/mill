package machine

import (
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = ginkgo.Describe("The nested grant", ginkgo.Label("unit"), func() {
	ginkgo.It("names a repository by its forge path however the origin spells it", func() {
		for _, origin := range []string{
			"https://gitlab.com/wellspring/mill.git",
			"https://oauth2:token@gitlab.com/wellspring/mill",
			"git@gitlab.com:wellspring/mill.git",
			"ssh://git@gitlab.com/wellspring/mill/",
		} {
			Expect(repoPath(origin)).To(Equal("wellspring/mill"), origin)
		}
		Expect(repoPath("wellspring/mill")).To(Equal("wellspring/mill"))
		Expect(repoPath("/wellspring/mill.git")).To(Equal("wellspring/mill"))
		Expect(repoPath("https://gitlab.com")).To(BeEmpty())

		w := &Workspace{config: Config{NestedRepos: []string{"Wellspring/mill", "https://gitlab.com/other/repo.git"}}}
		Expect(w.nestedGranted("https://gitlab.com/wellspring/mill.git")).To(BeTrue(), "by path, case aside")
		Expect(w.nestedGranted("git@gitlab.com:other/repo")).To(BeTrue(), "by URL")
		Expect(w.nestedGranted("https://gitlab.com/wellspring/mill-docs.git")).To(BeFalse())
		Expect(w.nestedGranted("")).To(BeFalse())
		empty := &Workspace{config: Config{NestedRepos: []string{""}}}
		Expect(empty.nestedGranted("https://gitlab.com")).To(BeFalse(), "an empty grant grants nothing")
	})
})
