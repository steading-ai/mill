package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/testsupport"
)

// `mill serve --work` is the deployed shape: one process, both faces
// (docs/architecture.md §"One binary, two faces"). What has to hold of it is
// that the worker being in the process does not cost the service anything —
// the socket still answers, the ledger still reads.

var _ = Describe("mill serve --work", Label(testsupport.DBLabel), func() {
	It("serves the store while a worker claims in the same process", func(ctx SpecContext) {
		g := daggerNode(ctx)
		databaseURL := pg.Database(ctx).Config().ConnString()
		socket := filepath.Join(GinkgoT().TempDir(), "mill.sock")

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{
			"serve", "--work",
			"--socket", socket,
			"--database-url", databaseURL,
			// A configuration file that does not exist, so the flags on
			// this line are the whole of the worker's settings.
			"--config", filepath.Join(GinkgoT().TempDir(), "absent.yaml"),
			"--project", daggerProject,
			// A label no node carries, so the worker blocks in ClaimNext
			// for the whole spec: what is under test is the service beside
			// it, and the run in the ledger below is composed rather than
			// driven.
			"--label", "nothing-carries-this",
			"--dagger-socket", g.service.Socket,
			"--vcs-url", "https://forge.test/mill/mill.git",
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--log-dir", filepath.Join(GinkgoT().TempDir(), "logs"),
		})

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			served <- root.ExecuteContext(serveCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(served, time.Minute).Should(Receive(BeNil()), stderr.String())
		})

		Eventually(func() error {
			_, err := os.Stat(socket)
			return err
		}, time.Minute, 50*time.Millisecond).Should(Succeed(), stderr.String())
		Eventually(stderr.String, time.Minute, 100*time.Millisecond).
			Should(ContainSubstring("worker starting in this process"))

		By("a run in progress")
		// Composed through the same Connect API a worker writes it with:
		// what is being specified here is that the service answers while a
		// worker is in the process, not what the worker does to a node.
		r := openRunOn(ctx, mill{socket: socket}, g.nodeRef, g.nodeID)
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))

		By("mill status answers")
		out, err := runCLI(ctx, socket, "status")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring(g.nodeRef))

		By("and mill doctor's service probes pass")
		// The sidecar, the signing key and the forge credential are the
		// host's and are not configured under a spec; what this asserts is
		// that a worker in the process costs the service none of its own
		// probes.
		report, _ := runCLI(ctx, socket, "doctor",
			"--dagger-socket", g.service.Socket, "--project", daggerProject,
			"--sidecar", filepath.Join(GinkgoT().TempDir(), "no-sidecar.js"),
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--vcs-url", "")
		for _, name := range []string{"socket", "schema", "dagger"} {
			Expect(probeLine(report, name)).To(Equal("pass"), report)
		}
	})
})

// probeLine is doctor's verdict for one probe.
func probeLine(output, name string) string {
	for _, line := range lines(output) {
		if fields := strings.Fields(line); len(fields) > 2 && fields[1] == name {
			return fields[0]
		}
	}
	return ""
}
