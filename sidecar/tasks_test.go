package sidecar_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
)

// The wait a session ends its turn on is watched: every heartbeat interval
// the sidecar looks at the task's output, asks the session about a task that
// has gone silent, and after the last heartbeat kills what it would not stop
// (sidecar/src/tasks.ts). The interval is three minutes in the contract and
// is shortened here, because a spec cannot wait three of them; nothing else
// about the shape is changed.
const (
	// heartbeatVar shortens the wait between looks.
	heartbeatVar = "MILL_SIDECAR_HEARTBEAT_MS"
	// pidVar names the file the fake harness writes its background task's
	// process id to.
	pidVar = "MILL_FAKE_PID"
	// workVar is how long the fake's working task keeps writing output.
	workVar = "MILL_FAKE_WORK_MS"
)

// heartbeats are the heartbeat events the sidecar wrote.
func heartbeats(s *sidecar) []*harnessv1.Heartbeat {
	beats := []*harnessv1.Heartbeat{}
	for _, event := range s.events() {
		if beat := event.GetHeartbeat(); beat != nil {
			beats = append(beats, beat)
		}
	}
	return beats
}

// results are the tool results the sidecar wrote, by call id.
func results(s *sidecar) map[string]string {
	out := map[string]string{}
	for _, event := range s.events() {
		if result := event.GetToolResult(); result != nil {
			out[result.GetCallId()] = result.GetContent()
		}
	}
	return out
}

// spawnedPid is the process id the fake harness spawned for its background
// task.
func spawnedPid(path string) int {
	GinkgoHelper()

	body, err := os.ReadFile(path) //nolint:gosec // a spec's own scratch file
	Expect(err).NotTo(HaveOccurred())
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	Expect(err).NotTo(HaveOccurred())
	return pid
}

// alive reports whether a process is still running. A process that has been
// killed but not yet reaped still answers a signal, so its state is read
// rather than signalled for: the state follows the parenthesised command
// name, which may itself contain spaces, so the last ')' is where it starts.
func alive(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	end := strings.LastIndex(string(stat), ")")
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(stat)[end+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

var _ = Describe("A session that ends its turn with a task running", Label("unit"), func() {
	It("A silent task is asked about, then stopped", func() {
		By("given a session that ended its turn with a background task producing no output")
		pidFile := filepath.Join(GinkgoT().TempDir(), "task.pid")
		cwd := GinkgoT().TempDir()
		line := startRequest("stall-1", func(start *harnessv1.Start) { start.Cwd = cwd })

		By("when three heartbeats pass without the session stopping it")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "silent-task",
			pidVar:               pidFile,
			heartbeatVar:         "300",
		}, line)

		By("then the task's process is killed")
		pid := spawnedPid(pidFile)
		Eventually(func() bool { return alive(pid) }, 10*time.Second).Should(BeFalse())

		By("and the step's final message is the last working turn's text")
		Expect(s.terminal().GetFinalMessage()).To(Equal("Started the suite and I am waiting on it."))

		By("and the attempt's exit reason is stalled")
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_STALLED))
		Expect(s.terminal().GetExit().GetStatus()).To(Equal(int32(1)))

		By("and the two heartbeat turns are in the event stream")
		beats := heartbeats(s)
		Expect(beats).To(HaveLen(2))
		Expect(beats[0].GetStrike()).To(Equal(int32(1)))
		Expect(beats[1].GetStrike()).To(Equal(int32(2)))
		Expect(beats[0].GetStrikes()).To(Equal(int32(3)))
		Expect(beats[0].GetTasks()).To(ConsistOf("Bash: make test"))
	})

	It("A working task is left alone", func() {
		By("given a session that ended its turn with a task whose output keeps growing")
		cwd := GinkgoT().TempDir()
		line := startRequest("stall-2", func(start *harnessv1.Start) { start.Cwd = cwd })

		By("when ten minutes pass")
		// Ten minutes is more than three heartbeats' worth of waiting, and
		// what the spec needs from it is exactly that: the wait outlasts the
		// breaker several times over, with the interval shortened so it does
		// so in seconds.
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "working-task",
			heartbeatVar:         "200",
			workVar:              "3000",
		}, line)

		By("then no heartbeat was sent")
		Expect(heartbeats(s)).To(BeEmpty())

		By("and the session waited until the task ended")
		Expect(s.terminal().GetFinalMessage()).To(Equal("The suite passed."))
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED))
	})

	It("ends normally when the session answers the first heartbeat by stopping the task", func() {
		By("given a silent task whose session stops it when it is asked")
		pidFile := filepath.Join(GinkgoT().TempDir(), "task.pid")
		cwd := GinkgoT().TempDir()
		line := startRequest("stall-3", func(start *harnessv1.Start) { start.Cwd = cwd })

		By("when the sidecar sends the first heartbeat")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "answered-heartbeat",
			pidVar:               pidFile,
			heartbeatVar:         "300",
		}, line)

		By("then one heartbeat was sent and the breaker never tripped")
		Expect(heartbeats(s)).To(HaveLen(1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED))
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())

		By("and the step's final message is the working turn's text, not the heartbeat's answer")
		Expect(s.terminal().GetFinalMessage()).To(Equal("Started the suite and I am waiting on it."))
	})

	It("The self-matching wait never runs", func() {
		By("given a session that asks to run a pgrep loop")
		line := startRequest("hook-1", nil)

		By("when the tool call is checked")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "poll-loop"}, line)

		By("then it is denied and the reason names TaskOutput")
		checked := results(s)
		Expect(checked).To(HaveKey("toolu_loop"))
		Expect(checked["toolu_loop"]).To(HavePrefix("denied:"))
		Expect(checked["toolu_loop"]).To(ContainSubstring("TaskOutput"))

		By("and a pgrep outside a loop is allowed")
		Expect(checked).To(HaveKey("toolu_plain"))
		Expect(checked["toolu_plain"]).To(Equal("allowed"))
	})

	It("The shell waits as long as the step may", func() {
		By("given a step with timeout 20m")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("budget-1", func(start *harnessv1.Start) {
			start.TimeoutMs = 20 * 60 * 1000
		})

		By("when the sidecar starts its session")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the session's default shell timeout is 20m")
		env, ok := applied(options)["env"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no environment")
		Expect(env).To(HaveKeyWithValue("BASH_DEFAULT_TIMEOUT_MS", "1200000"))
		Expect(env).To(HaveKeyWithValue("BASH_MAX_TIMEOUT_MS", "1200000"), "the harness's own ceiling would clamp a budget past ten minutes")

		By("and the sidecar's startup line reports the value it set")
		Expect(s.stderr.String()).To(MatchRegexp(`(?m)^sidecar: session ` + sessionSeed + `: starting under a 20m step; BASH_DEFAULT_TIMEOUT_MS=1200000 BASH_MAX_TIMEOUT_MS=1200000$`))
	})

	It("gives every step the tools its wait needs, and a cap on a foreground command", func() {
		By("given a step whose tool policy names neither task tool")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("tasks-1", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"tools": map[string]any{"deny": []any{"Write"}}})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then TaskOutput and TaskStop are in the allowed tools")
		applied := applied(options)
		Expect(applied["allowedTools"]).To(ContainElements("TaskOutput", "TaskStop"))
		Expect(applied["disallowedTools"]).To(Equal([]any{"Write"}))

		By("and the session's environment carries the step's budget as the foreground cap")
		env, ok := applied["env"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no environment")
		Expect(env).To(HaveKeyWithValue("BASH_DEFAULT_TIMEOUT_MS", strconv.Itoa(stepTimeoutMs)))
	})

	It("leaves the task tools out when the step denies them", func() {
		By("given a step that denies TaskStop")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("tasks-2", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"tools": map[string]any{"deny": []any{"TaskStop"}}})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the denied tool is not allowed back in")
		applied := applied(options)
		Expect(applied["allowedTools"]).To(Equal([]any{"TaskOutput"}))
		Expect(applied["disallowedTools"]).To(Equal([]any{"TaskStop"}))
	})
})
