package claudecode

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/harness"
)

// liveVar turns on the spec that spends a real session against the account's
// own Claude Code login. It is off by default: `make test` must not depend on
// a credential or on the model being reachable.
const liveVar = "MILL_SIDECAR_LIVE"

var _ = Describe("The claudecode adapter against the real sidecar", Label("live"), func() {
	var entrypoint string

	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 to run the specs that spend a real session")
		}

		root, err := filepath.Abs(filepath.Join("..", "..", "..", "sidecar"))
		Expect(err).NotTo(HaveOccurred())
		build := exec.Command("pnpm", "run", "build")
		build.Dir = root
		build.Stdout, build.Stderr = GinkgoWriter, GinkgoWriter
		Expect(build.Run()).To(Succeed(), "the sidecar did not build")
		entrypoint = filepath.Join(root, "dist", "main.js")
	})

	It("streams the session's events before the terminal record, and returns what it billed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		DeferCleanup(cancel)
		log := &recorder{}
		adapter := New(WithEntrypoint("node", entrypoint), WithTimeout(4*time.Minute))

		turn, err := adapter.Start(ctx, harness.Session{
			ID:     newSessionID(),
			Cwd:    GinkgoT().TempDir(),
			Events: log.sink(),
		}, harness.Prompt{Text: "Reply with the single word: ready. Use no tools."}, nil, harness.Policy{})
		Expect(err).NotTo(HaveOccurred())
		log.watch(turn)

		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)
		Expect(log.collected()).NotTo(BeEmpty(), "the turn streamed no events")
		Expect(log.afterTerminal()).To(BeZero(), "an event arrived after the terminal record")
		Expect(terminal.Usage).NotTo(BeEmpty(), "the adapter is the only component that sees the harness's numbers")
		for model, counts := range terminal.Usage {
			Expect(counts.Output).To(BeNumerically(">", 0), "model %s billed no output tokens", model)
		}
	})
})

// newSessionID mints the kind of id a run assigns a session: a UUID the
// harness accepts and has never seen.
func newSessionID() string {
	GinkgoHelper()

	raw := make([]byte, 16)
	_, err := rand.Read(raw)
	Expect(err).NotTo(HaveOccurred())
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
