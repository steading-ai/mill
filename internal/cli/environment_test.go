package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
)

// A face run in-process reads the mill's own environment: the dagger project
// it claims from, dagger's socket, the forge credential, the configuration
// file. That is right for a deployed mill and wrong for a spec, which is
// asserting about the command line it wrote down and not about the shell it
// happens to be run from. Under `mise exec` the project variable names the
// live project, and a `work` that finds a project there claims real nodes.
//
// So every spec in this suite runs with those variables removed, through the
// one helper below, and what a face sees is what its spec passed it.

// workerEnvironment is every variable a face reads for a setting a spec
// means to control. They are spelled here rather than taken from the package
// under test because what a spec removes from the process is a name, and a
// name the command stopped reading is one this list should stop naming too.
var workerEnvironment = []string{
	"DG_PROJECT",
	"DG_SOCKET",
	"MILL_VCS_URL",
	"MILL_FORGE_TOKEN",
	"MILL_FORGE_USER",
	"MILL_CONFIG",
}

// scrubEnvironment removes those variables for the rest of the current spec
// and puts back whatever the process held.
func scrubEnvironment() {
	GinkgoHelper()
	for _, name := range workerEnvironment {
		value, held := os.LookupEnv(name)
		Expect(os.Unsetenv(name)).To(Succeed())
		if held {
			DeferCleanup(os.Setenv, name, value)
		}
	}
}

// setEnvironment is a shell set up for the live mill: the project it works,
// dagger's socket, and the forge credential a workspace pushes with. The
// socket is a path nothing listens on, so a command that got as far as
// asking dagger for work fails rather than claiming.
func setEnvironment() {
	GinkgoHelper()
	for name, value := range map[string]string{
		"DG_PROJECT":       "mill",
		"DG_SOCKET":        filepath.Join(GinkgoT().TempDir(), "dg.sock"),
		"MILL_VCS_URL":     "https://forge.test/mill/mill.git",
		"MILL_FORGE_TOKEN": "not-a-real-token",
		"MILL_FORGE_USER":  "mill",
		"MILL_CONFIG":      filepath.Join(GinkgoT().TempDir(), "from-the-environment.yaml"),
	} {
		GinkgoT().Setenv(name, value)
	}
}

// Every spec in this suite, whichever file it is in, runs with those
// variables removed.
var _ = BeforeEach(scrubEnvironment)

var _ = Describe("A face's environment", Label("unit"), func() {
	var stdout, stderr bytes.Buffer

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	// Nothing is claimed and nothing is dialled, so the command returns
	// promptly; a worker that reached its claim loop would not.
	invoke := func(args ...string) int {
		GinkgoHelper()
		exit := make(chan int, 1)
		go func() {
			defer GinkgoRecover()
			exit <- cli.Run(args, strings.NewReader(""), &stdout, &stderr)
		}()
		var status int
		Eventually(exit, 30*time.Second).Should(Receive(&status), "the command did not return")
		return status
	}

	It("A unit spec starts no worker whatever the environment names", func() {
		By("given DG_PROJECT and the forge variables are set in the environment")
		setEnvironment()

		By("when the cli unit suite runs")
		// What every spec in the suite does before it runs a face. The
		// configuration file exists and is empty, so the projects the
		// worker would claim from can only have come from the environment.
		scrubEnvironment()
		config := filepath.Join(GinkgoT().TempDir(), "empty.yaml")
		Expect(os.WriteFile(config, []byte("{}\n"), 0o600)).To(Succeed())
		exit := invoke("work", "--config", config)

		By("then no lease is taken in dagger and the suite finishes")
		Expect(exit).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring("dagger project is required"),
			"the worker found no project, so it never asked dagger for one")
		Expect(stderr.String()).To(ContainSubstring("DG_PROJECT"))
		Expect(stderr.String()).NotTo(ContainSubstring("mill work starting"))
	})

	It("A config path that does not exist is refused", func() {
		By("given --config names a file that does not exist")
		// The environment is the live one, unscrubbed: the refusal has to
		// hold whatever it names, because it is what stops a worker
		// starting on it.
		setEnvironment()
		path := filepath.Join(GinkgoT().TempDir(), "absent.yaml")

		By("when mill work starts")
		exit := invoke("work", "--config", path)

		By("then it exits non-zero naming the path")
		Expect(exit).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring(path))
		Expect(stderr.String()).To(ContainSubstring("does not exist"))
		Expect(stderr.String()).NotTo(ContainSubstring("mill work starting"),
			"the refusal came before the worker was built, so nothing was claimed")
	})

	It("refuses a configuration file $MILL_CONFIG names and nothing is at", func() {
		setEnvironment()
		path := os.Getenv("MILL_CONFIG")
		Expect(invoke("work")).NotTo(BeZero())
		Expect(stderr.String()).To(ContainSubstring(path))
		Expect(stderr.String()).To(ContainSubstring("MILL_CONFIG"))
	})
})
