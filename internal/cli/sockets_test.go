package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/cli"
	"github.com/steading-ai/mill/internal/testsupport"
)

// The configuration file's `socket` and `skift-socket` reach every face
// that dials: a person whose file names the paths types no flag and exports
// no variable.
var _ = Describe("The configuration file's sockets", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	// bare runs a command with no --socket and nothing else runCLI adds:
	// what the file says is all the command has.
	bare := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(ctx)
		return stdout.String() + stderr.String(), err
	}

	// Scenario: The Mac-side CLI needs no environment
	//   Given mill's config file on the Mac names the forwarded socket paths
	//   And no MILL_SOCKET or SKIFT_SOCKET variable is set
	//   When "mill doctor" runs
	//   Then it connects to the socket named in the file
	It("The Mac-side CLI needs no environment", func() {
		By("given mill's config file names the forwarded socket paths, and no MILL_SOCKET or SKIFT_SOCKET is set")
		m := serving(ctx)
		skiftSocket := filepath.Join(GinkgoT().TempDir(), "forwarded-skift.sock")
		home := GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(home, "mill"), 0o750)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(home, "mill", "config.yaml"),
			[]byte("socket: "+m.socket+"\nskift-socket: "+skiftSocket+"\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("XDG_CONFIG_HOME", home)
		Expect(os.Getenv("MILL_SOCKET")).To(BeEmpty())
		Expect(os.Getenv("SKIFT_SOCKET")).To(BeEmpty())

		By("when mill doctor runs with no flag naming either socket")
		out, _ := bare("doctor",
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--project", "millspec")

		By("then it connects to the socket named in the file, and asks skift at the file's path")
		Expect(probeLine(out, "socket")).To(Equal("pass"), out)
		Expect(probeReason(out, "socket")).To(ContainSubstring(m.socket))
		Expect(probeReason(out, "skift")).To(ContainSubstring(skiftSocket), "skift was asked at the file's path")

		By("and mill status reaches the same service")
		out, err := bare("status")
		Expect(err).NotTo(HaveOccurred(), out)
	})
})

// probeReason is the doctor's reason for one probe.
func probeReason(output, name string) string {
	for _, line := range lines(output) {
		if fields := strings.Fields(line); len(fields) > 2 && fields[1] == name {
			return strings.Join(fields[2:], " ")
		}
	}
	return ""
}
