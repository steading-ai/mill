package mill_test

import (
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The unit file is what a deployment installs, so what it sets is part of
// the design and not an example. The one setting asserted here is the one a
// deployment cannot be allowed to forget: a mill started from a unit that
// carries DATABASE_URL and MILL_FORGE_TOKEN hands both to every session it
// spawns unless it is told not to.
var _ = Describe("The systemd unit", Label("unit"), func() {
	It("runs the worker under the strict environment mode", func() {
		body, err := os.ReadFile("contrib/systemd/mill.service")
		Expect(err).NotTo(HaveOccurred())

		Expect(string(body)).To(ContainSubstring("--env-mode strict"))
	})
})

// The example notify command. It is what a deployment points
// --notify-command at, so what is asserted here is its contract with the
// mill: it reads the notice on standard input, it tells a probe from a gate,
// and it ends with `thicket send`'s status (docs/architecture.md §"Gate
// steps").
var _ = Describe("The example notify command", Label("unit"), func() {
	const script = "contrib/notify/thicket-send.sh"

	It("is an executable shell script that parses", func() {
		info, err := os.Stat(script)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()&0o111).NotTo(BeZero(), "a notify command mill spawns must be executable")

		out, err := exec.Command("bash", "-n", script).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
	})

	It("renders the notice for a thicket agent, and exits with thicket send's status", func() {
		body, err := os.ReadFile(script)
		Expect(err).NotTo(HaveOccurred())
		text := string(body)

		Expect(text).To(ContainSubstring(`thicket send "$agent" -`), "the message goes in on stdin")
		Expect(text).To(ContainSubstring(`${THICKET_AGENT:-ops}`), "which agent is the deployment's to say")
		Expect(text).To(ContainSubstring(`%s gate %s on %s: %s`), "kind, gate, node and title")
		Expect(text).To(ContainSubstring(`"$kind" = "probe"`), "a doctor probe is not a person's decision")
		Expect(strings.Contains(text, "set -euo pipefail")).To(BeTrue())
	})
})
