package mill_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("The build", Label("unit", "slow"), func() {
	It("make build produces a binary that reports its version", func() {
		dir := checkout()

		By("when make build runs")
		Expect(makeTarget(dir, "build")).To(gexec.Exit(0))

		By("then ./mill exists")
		binary := filepath.Join(dir, "mill")
		info, err := os.Stat(binary)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm() & 0o111).NotTo(BeZero())

		By("and ./mill version prints a version")
		session, err := gexec.Start(exec.Command(binary, "version"), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Wait(30 * time.Second)).To(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(MatchRegexp(`^mill version \d+\.\d+\.\d+`))
	})
})
