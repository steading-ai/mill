package claude

import (
	"context"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/workspace"
)

// twoModels is a terminal record of the shape a real turn produces: the
// model the step asked for, and the auxiliary model the same turn billed.
const twoModels = `{"terminal":{"request_id":"","final_message":"implemented the node",` +
	`"usage":{"by_model":{` +
	`"claude-opus-5":{"input":"4","output":"150","cache_read":"25513","cache_write":"25789"},` +
	`"claude-haiku-4-5-20251001":{"input":"3870","output":"76","cache_read":"0","cache_write":"0"}` +
	`}},"exit":{"status":0}}}`

var _ = Describe("The claude adapter", Label("unit"), func() {
	var (
		log      *recorder
		spawner  *watching
		session  harness.Session
		lifetime context.Context //nolint:containedctx // the spec's own deadline, held so every It shares it
	)

	BeforeEach(func() {
		log = &recorder{}
		spawner = &watching{Execer: workspace.Spawner{}}
		session = harness.Session{
			ID:        "b6f1e0a2-0000-4000-8000-000000000001",
			Options:   map[string]any{"model": "claude-opus-5", "effort": "high"},
			Cwd:       GinkgoT().TempDir(),
			Workspace: harness.Workspace{Socket: "/run/mill/ws.sock", Token: "step-token-1"},
			Timeout:   20 * time.Minute,
			Events:    log.sink(),
		}
		var cancel context.CancelFunc
		lifetime, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	// adapter is the adapter under test, spawning the given stub script.
	adapter := func(script string, options ...Option) *Adapter {
		return New(append([]Option{
			WithExecer(spawner),
			WithEntrypoint("/bin/sh", script),
		}, options...)...)
	}

	It("Usage from the terminal record is the adapter's return value", func() {
		By("given a sidecar terminal record billing claude-opus-5 and claude-haiku-4-5")
		script := stub(`
printf '%s\n' '{"event":{"text":{"text":"working"}}}'
printf '%s\n' 'TERMINAL'
`, "TERMINAL", twoModels)

		By("when the adapter returns the Turn")
		turn, err := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		log.watch(turn)
		terminal, err := turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		By("then Terminal.Usage has both models with input, output, cache_read, cache_write counts")
		Expect(terminal.Usage.Models()).To(Equal([]string{"claude-haiku-4-5-20251001", "claude-opus-5"}))
		Expect(terminal.Usage["claude-opus-5"]).To(Equal(harness.TokenCounts{
			Input: 4, Output: 150, CacheRead: 25513, CacheWrite: 25789,
		}))
		Expect(terminal.Usage["claude-haiku-4-5-20251001"]).To(Equal(harness.TokenCounts{
			Input: 3870, Output: 76, CacheRead: 0, CacheWrite: 0,
		}))
		Expect(terminal.FinalMessage).To(Equal("implemented the node"))
		Expect(terminal.Exit).To(Equal(harness.ProcessExit{Status: 0, Reason: harness.ReasonNone}))

		By("and every event reached the sink before the turn ended")
		Expect(log.collected()).To(HaveLen(1))
		Expect(log.afterTerminal()).To(BeZero())
	})

	It("Interrupt, grace, kill", func() {
		By("given a stub sidecar that traps SIGTERM and sleeps")
		pidFile := scratch("child.pid")
		script := stub(`
trap 'echo "ignoring TERM" >&2' TERM
/bin/sh -c 'trap "" TERM; sleep 600' &
echo $! > PIDFILE
printf '%s\n' '{"event":{"text":{"text":"ready"}}}'
while :; do sleep 0.1; done
`, "PIDFILE", pidFile)

		running := adapter(script, WithGrace(2*time.Second))
		turn, err := running.Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		Eventually(log.collected).Should(HaveLen(1), "the stub never reported itself ready")
		group := spawner.pid()
		child := readPid(pidFile)

		By("when Cancel is called with a 2s grace")
		started := time.Now()
		Expect(running.Cancel(lifetime, session)).To(Succeed())
		elapsed := time.Since(started)

		By("then SIGKILL is sent at 2s and the process group has no members at 3s")
		Expect(elapsed).To(BeNumerically(">=", 1900*time.Millisecond), "the sidecar was killed before its grace ran out")
		Expect(elapsed).To(BeNumerically("<", 3*time.Second))
		Eventually(func() bool { return gone(-group) }, time.Second, 20*time.Millisecond).Should(BeTrue())
		Eventually(func() bool { return gone(child) }, time.Second, 20*time.Millisecond).Should(BeTrue(), "the sidecar left an orphan")

		terminal, err := turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Exit.Status).To(BeNumerically("==", -1))
		Expect(terminal.Exit.Reason).To(Equal(harness.ReasonCancelled))
		Expect(terminal.Exit.Signal).To(BeNumerically("==", 9))
	})

	It("carries the step's timeout to the sidecar as timeout_ms, on a start and on a resume", func() {
		By("given a step with a 20m timeout and a stub sidecar that keeps its request")
		kept := scratch("request.json")
		script := stub(`
head -n 1 > KEPT
printf '%s\n' 'TERMINAL'
`, "KEPT", kept, "TERMINAL", twoModels)
		session.Timeout = 20 * time.Minute

		By("when the adapter starts the session")
		turn, err := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		By("then the start request carries the timeout in milliseconds")
		Expect(readRequest(kept).GetStart().GetTimeoutMs()).To(Equal(int32(20 * 60 * 1000)))

		By("and so does a resume")
		turn, err = adapter(script).Resume(lifetime, session, harness.Prompt{Text: "revise"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())
		Expect(readRequest(kept).GetResume().GetTimeoutMs()).To(Equal(int32(20 * 60 * 1000)))
	})

	It("refuses a step whose timeout is zero before spawning anything", func() {
		By("given a session with no timeout")
		session.Timeout = 0
		script := stub(`printf '%s\n' 'TERMINAL'`, "TERMINAL", twoModels)

		By("when the adapter is asked to start or resume it")
		_, startErr := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		_, resumeErr := adapter(script).Resume(lifetime, session, harness.Prompt{Text: "revise"}, nil)

		By("then both are refused, naming the session, and no sidecar was spawned")
		Expect(startErr).To(MatchError(ContainSubstring("no timeout")))
		Expect(startErr).To(MatchError(ContainSubstring(session.ID)))
		Expect(resumeErr).To(MatchError(ContainSubstring("no timeout")))
		Expect(spawner.spawned()).To(BeEmpty())
	})

	It("cancels the session when the step's timeout elapses, and reports it as a timeout", func() {
		script := stub(`
trap '' TERM
printf '%s\n' '{"event":{"text":{"text":"ready"}}}'
while :; do sleep 0.1; done
`)
		grace := 500 * time.Millisecond

		started := time.Now()
		session.Timeout = 300 * time.Millisecond
		turn, err := adapter(script, WithGrace(grace)).Start(
			lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		group := spawner.pid()
		terminal, err := turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		Expect(terminal.Exit.Status).To(BeNumerically("==", -1))
		Expect(terminal.Exit.Reason).To(Equal(harness.ReasonTimeout))
		Expect(time.Since(started)).To(BeNumerically("<", 300*time.Millisecond+grace+time.Second))
		Eventually(func() bool { return gone(-group) }, time.Second, 20*time.Millisecond).Should(BeTrue())
	})

	It("reports what a killed session billed, from the usage it streamed", func() {
		script := stub(`
trap '' TERM
printf '%s\n' '{"event":{"usage":{"by_model":{"claude-opus-5":{"input":"4","output":"150"}}}}}'
printf '%s\n' '{"event":{"usage":{"by_model":{"claude-opus-5":{"input":"6","output":"50"}}}}}'
while :; do sleep 0.1; done
`)

		session.Timeout = 300 * time.Millisecond
		turn, err := adapter(script, WithGrace(300*time.Millisecond)).Start(
			lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(lifetime)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Exit.Reason).To(Equal(harness.ReasonTimeout))
		Expect(terminal.Usage).To(Equal(harness.Usage{"claude-opus-5": {Input: 10, Output: 200}}))
	})

	It("reports a sidecar that exits without a terminal line as refused, with what it said", func() {
		script := stub(`
echo "sidecar: cannot reach the workspace socket" >&2
exit 1
`)

		turn, err := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(lifetime)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Exit).To(Equal(harness.ProcessExit{Status: -1, Reason: harness.ReasonRefused}))
		Expect(terminal.FinalMessage).To(Equal("sidecar: cannot reach the workspace socket"))
	})

	// A Node crash's last line is always its version, so a diagnostic that
	// kept one line kept the least useful one. The whole stream is kept for
	// the step's log, and the message is the tail of it.
	It("keeps a crashed sidecar's whole stderr, and says its last lines", func() {
		script := stub(`
i=1
while [ $i -le 40 ]; do echo "    at frame$i (main.js:$i:1)" >&2; i=$((i+1)); done
echo "Node.js v24.4.1" >&2
exit 1
`)

		turn, err := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		// The message is the last twenty lines: the version line, and the
		// nineteen frames before it.
		lines := strings.Split(terminal.FinalMessage, "\n")
		Expect(lines).To(HaveLen(20))
		Expect(lines[len(lines)-1]).To(Equal("Node.js v24.4.1"))
		Expect(lines[0]).To(Equal("    at frame22 (main.js:22:1)"))

		// The diagnostics are all of it, which is what reaches the step's
		// log file.
		Expect(terminal.Diagnostics).To(ContainSubstring("at frame1 (main.js:1:1)"))
		Expect(terminal.Diagnostics).To(ContainSubstring("at frame40 (main.js:40:1)"))
	})

	It("bounds what it keeps at 64 KiB, and says the front was dropped", func() {
		// Two hundred thousand bytes of it: more than the bound, in lines
		// long enough that the count is not the interesting part.
		script := stub(`
i=1
while [ $i -le 2000 ]; do
  printf 'line %04d %s\n' "$i" "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" >&2
  i=$((i+1))
done
exit 1
`)

		turn, err := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		Expect(len(terminal.Diagnostics)).To(BeNumerically("<", 70<<10))
		Expect(terminal.Diagnostics).To(ContainSubstring("were dropped"))
		Expect(terminal.Diagnostics).To(ContainSubstring("line 2000"))
		Expect(terminal.Diagnostics).NotTo(ContainSubstring("line 0001"))
	})

	// What the child's environment is made of is environment_test.go: it is
	// a mode's decision now rather than a fixed pair, and the socket and the
	// token are what survives every mode.

	It("hands the sidecar one request line carrying the session, prompt, settings and workspace", func() {
		requestFile := scratch("request.json")
		script := stub(`
read -r line
printf '%s' "$line" > REQUEST
printf '%s\n' '{"terminal":{"exit":{"status":0}}}'
`, "REQUEST", requestFile)

		turn, err := adapter(script).Start(lifetime, session,
			harness.Prompt{Text: "implement the node", OutputSchema: `{"type":"object"}`},
			[]harness.Attachment{{Name: "dagger#3", Source: "predecessor", MediaType: "text/markdown", Content: "done"}})
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		raw, err := os.ReadFile(requestFile) //nolint:gosec // a spec's own scratch file
		Expect(err).NotTo(HaveOccurred())
		request := &harnessv1.Request{}
		Expect(protojson.Unmarshal(raw, request)).To(Succeed())

		Expect(request.GetId()).To(HavePrefix("mill-"))
		start := request.GetStart()
		Expect(start).NotTo(BeNil())
		Expect(start.GetSessionId()).To(Equal(session.ID))
		Expect(start.GetPrompt()).To(Equal("implement the node"))
		Expect(start.GetOutputSchema()).To(Equal(`{"type":"object"}`))
		Expect(start.GetCwd()).To(Equal(session.Cwd))
		Expect(start.GetSettings().GetOptions().AsMap()).To(Equal(session.Options))
		Expect(start.GetWorkspace().GetSocket()).To(Equal("/run/mill/ws.sock"))
		Expect(start.GetWorkspace().GetToken()).To(Equal("step-token-1"))
		Expect(start.GetAttachments()).To(HaveLen(1))
		Expect(start.GetAttachments()[0].GetSource()).To(Equal("predecessor"))
	})

	It("Mill does not interpret the options", func() {
		By("given a step whose claude options carry model, effort and tools")
		document := map[string]any{
			"model":  "claude-opus-5",
			"effort": "high",
			"tools":  map[string]any{"allow": []any{"Read"}, "deny": []any{"Write"}},
			// A key this adapter has never heard of. The sidecar owns the
			// schema; the adapter's job is to hand the document over whole.
			"thinking_budget": float64(4096),
		}
		session.Options = document

		requestFile := scratch("request.json")
		script := stub(`
read -r line
printf '%s' "$line" > REQUEST
printf '%s\n' '{"terminal":{"exit":{"status":0}}}'
`, "REQUEST", requestFile)

		By("when the adapter builds the Start request")
		turn, err := adapter(script).Start(lifetime, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		By("then the request's options equal the step's options document")
		raw, err := os.ReadFile(requestFile) //nolint:gosec // a spec's own scratch file
		Expect(err).NotTo(HaveOccurred())
		request := &harnessv1.Request{}
		Expect(protojson.Unmarshal(raw, request)).To(Succeed())
		Expect(request.GetStart().GetSettings().GetOptions().AsMap()).To(Equal(document))
	})

	It("sends Resume for a session that has run before", func() {
		requestFile := scratch("request.json")
		script := stub(`
read -r line
printf '%s' "$line" > REQUEST
printf '%s\n' '{"terminal":{"exit":{"status":0}}}'
`, "REQUEST", requestFile)

		turn, err := adapter(script).Resume(lifetime, session, harness.Prompt{Text: "the review found this"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(lifetime)
		Expect(err).NotTo(HaveOccurred())

		raw, err := os.ReadFile(requestFile) //nolint:gosec // a spec's own scratch file
		Expect(err).NotTo(HaveOccurred())
		request := &harnessv1.Request{}
		Expect(protojson.Unmarshal(raw, request)).To(Succeed())
		Expect(request.GetResume().GetSessionId()).To(Equal(session.ID))
		Expect(request.GetResume().GetPrompt()).To(Equal("the review found this"))
	})

	It("cancels a session that is not running without complaint", func() {
		Expect(New(WithExecer(spawner)).Cancel(lifetime, session)).To(Succeed())
		Expect(spawner.spawned()).To(BeEmpty())
	})

	It("refuses to start a session that is already running", func() {
		script := stub(`
printf '%s\n' '{"event":{"text":{"text":"ready"}}}'
while :; do sleep 0.1; done
`)
		running := adapter(script, WithGrace(200*time.Millisecond))
		_, err := running.Start(lifetime, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		Eventually(log.collected).Should(HaveLen(1))
		DeferCleanup(func() { Expect(running.Cancel(lifetime, session)).To(Succeed()) })

		_, err = running.Start(lifetime, session, harness.Prompt{}, nil)

		Expect(err).To(MatchError(ContainSubstring("already running")))
	})
})
