package sidecar_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
)

// The numbers the fake harness reports for the turn it plays, mirroring
// spec/fake_sdk.ts, which in turn mirrors the per-model bill observed in
// docs/spikes/sdk-sessions.md §Q4. The terminal record must carry them to
// the token: the harness's own report is the authority, and the sidecar
// translates it rather than recomputing it.
var billed = map[string]*harnessv1.TokenCounts{
	"claude-opus-5":             {Input: 4, Output: 150, CacheRead: 25513, CacheWrite: 25789},
	"claude-haiku-4-5-20251001": {Input: 3870, Output: 76, CacheRead: 0, CacheWrite: 0},
}

// reviewSchema is a structured-output schema of the shape a reviewer step
// declares: an enum, a required array, and a nested object.
const reviewSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["decision", "findings", "summary"],
  "properties": {
    "decision": {"type": "string", "enum": ["approve", "revise", "needs_human"]},
    "findings": {"type": "array", "items": {"type": "object"}},
    "summary": {
      "type": "object",
      "required": ["headline", "confidence"],
      "properties": {"headline": {"type": "string"}, "confidence": {"type": "number"}}
    }
  }
}`

// applied is the options the fake harness was handed, as the sidecar
// compiled them.
func applied(path string) map[string]any {
	GinkgoHelper()

	body, err := os.ReadFile(path) //nolint:gosec // a spec's own scratch file
	Expect(err).NotTo(HaveOccurred())
	options := map[string]any{}
	Expect(json.Unmarshal(body, &options)).To(Succeed())
	return options
}

// stepTimeoutMs is the budget the specs' steps run under: twenty minutes,
// which is what the feature workflow gives gather-context.
const stepTimeoutMs = 20 * 60 * 1000

func startRequest(id string, mutate func(*harnessv1.Start)) string {
	start := &harnessv1.Start{
		SessionId: sessionSeed,
		Prompt:    "Summarise the node body.",
		Cwd:       ".",
		TimeoutMs: stepTimeoutMs,
	}
	if mutate != nil {
		mutate(start)
	}
	return request(&harnessv1.Request{Id: id, Body: &harnessv1.Request_Start{Start: start}})
}

var _ = Describe("The sidecar", Label("unit"), func() {
	It("writes event lines and exactly one terminal record, then exits 0", func() {
		By("given a Start request with a trivial prompt")
		line := startRequest("mill-01JB-implement-1", nil)

		By("when the sidecar runs it")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, line)

		By("then it wrote at least one event, exactly one terminal record, and exited 0")
		Expect(len(s.events())).To(BeNumerically(">=", 1))
		Expect(s.terminals()).To(HaveLen(1))
		Expect(s.exitCode()).To(Equal(0))

		By("and the terminal record is the last line, carrying the session's final message")
		Expect(s.responses()[len(s.responses())-1].GetTerminal()).NotTo(BeNil())
		Expect(s.terminal().GetFinalMessage()).To(Equal("Read the node body and summarised it."))
		Expect(s.terminal().GetRequestId()).To(Equal("mill-01JB-implement-1"))
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())
	})

	It("reports the tokens of every model the harness billed", func() {
		By("given a session whose turn bills a second model beside the one the step asked for")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, startRequest("usage-1", nil))

		By("then the terminal usage map lists every model, with counts by kind, to the token")
		usage := s.terminal().GetUsage().GetByModel()
		Expect(usage).To(HaveLen(len(billed)))
		for model, counts := range billed {
			Expect(usage).To(HaveKey(model))
			Expect(usage[model].GetInput()).To(Equal(counts.GetInput()), model)
			Expect(usage[model].GetOutput()).To(Equal(counts.GetOutput()), model)
			Expect(usage[model].GetCacheRead()).To(Equal(counts.GetCacheRead()), model)
			Expect(usage[model].GetCacheWrite()).To(Equal(counts.GetCacheWrite()), model)
		}

		By("and the streamed usage events carry the turn that produced them")
		perTurn := []*harnessv1.Usage{}
		for _, event := range s.events() {
			if usage := event.GetUsage(); usage != nil {
				perTurn = append(perTurn, usage)
			}
		}
		Expect(perTurn).NotTo(BeEmpty())
		Expect(perTurn[0].GetByModel()).To(HaveKey("claude-opus-5"))
	})

	It("applies the tool policy the step gave it, to both of the SDK's knobs", func() {
		By("given a policy that allows reading and excludes Write and Edit")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("policy-1", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{
				"model":  "opus",
				"effort": "high",
				"tools": map[string]any{
					"allow": []any{"Read", "Grep"},
					"deny":  []any{"Write", "Edit"},
				},
			})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the harness was given the allowlist to permit with and the denylist to remove with")
		applied := map[string]any{}
		body, err := os.ReadFile(options)
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(body, &applied)).To(Succeed())
		// The task tools are in every step's allow list unless the step
		// denied them, so what the step asked for is what is here beside
		// them (sidecar/src/options.ts).
		Expect(applied["allowedTools"]).To(Equal([]any{"Read", "Grep", "TaskOutput", "TaskStop"}))
		Expect(applied["disallowedTools"]).To(Equal([]any{"Write", "Edit"}))
		Expect(applied["model"]).To(Equal("opus"))
		Expect(applied["effort"]).To(Equal("high"))
		Expect(applied["sessionId"]).To(Equal(sessionSeed))
		Expect(applied["settingSources"]).To(BeEmpty())
	})

	It("appends the worker's paragraph about the session's surroundings to the harness's own system prompt", func() {
		By("given a start whose system_prompt says how the session reaches out")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		egress := "Every outbound connection from this session goes through the proxy named by HTTPS_PROXY."
		line := startRequest("system-prompt-1", func(start *harnessv1.Start) {
			start.SystemPrompt = egress
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the harness keeps its own preset with the paragraph appended")
		Expect(applied(options)["systemPrompt"]).To(Equal(map[string]any{
			"type":   "preset",
			"preset": "claude_code",
			"append": egress,
		}))
	})

	It("leaves the harness's system prompt alone when the worker has nothing to add", func() {
		By("given a start with an empty system_prompt")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("system-prompt-2", nil)

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then no system prompt option is set")
		Expect(applied(options)).NotTo(HaveKey("systemPrompt"))
	})

	It("refuses settings it cannot apply without starting a session", func() {
		By("given a step asking for an effort level the harness has no name for")
		line := startRequest("policy-2", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"effort": "titanic"})
		})

		By("when the sidecar runs")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, line)

		By("then the session never ran, and the reason says the settings were the problem")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_POLICY))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("titanic"))
		Expect(s.events()).To(BeEmpty())
	})

	It("A knob the sidecar does not know is refused by the sidecar", func() {
		By("given a step whose claude options carry the key thinking_budget")
		line := startRequest("policy-3", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"model": "opus", "thinking_budget": 4096})
		})

		By("when the step starts")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, line)

		By("then the sidecar refuses with exit reason policy naming thinking_budget")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_POLICY))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("thinking_budget"))
		Expect(s.events()).To(BeEmpty())
	})

	It("runs a session under the model classifier when the step names no permission mode", func() {
		By("given a step whose claude options name no permission mode")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("permission-1", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"model": "sonnet"})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the harness was given permissionMode auto")
		Expect(applied(options)["permissionMode"]).To(Equal("auto"))
	})

	It("runs a session under the permission mode the step named", func() {
		By("given a step whose claude options name permission_mode acceptEdits")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("permission-2", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"permission_mode": "acceptEdits"})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the harness was given that mode")
		Expect(applied(options)["permissionMode"]).To(Equal("acceptEdits"))
	})

	It("Bypass is refused until there is a sandbox", func() {
		By("given a step whose claude options name permission_mode bypassPermissions")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("permission-3", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"permission_mode": "bypassPermissions"})
		})

		By("when the sidecar receives the Start request")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then it writes a terminal record with exit reason policy and starts no session")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_POLICY))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("bypassPermissions"))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("sandbox"))
		Expect(s.events()).To(BeEmpty())
		// The refusal happens before the harness is loaded at all, so the
		// fake never wrote the options it would have run under.
		_, err := os.Stat(options)
		Expect(os.IsNotExist(err)).To(BeTrue(), "the harness was loaded for a refused request")
	})

	It("gives the session what the step carved out of the environment", func() {
		By("given a step naming a variable to pass through and one to set")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("env-1", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"env": map[string]any{
				"passthrough": []any{"GOFLAGS"},
				"set":         map[string]any{"MILL_SPEC_LITERAL": "set-by-the-step"},
			}})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
			"GOFLAGS":            "-mod=mod",
			"MILL_SPEC_ABSENT":   "the sidecar has this and the session does not",
		}, line)

		By("then the session's environment holds the floor, the carve-out, and nothing else of the sidecar's")
		env, ok := applied(options)["env"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no environment")
		Expect(env).To(HaveKey("PATH"))
		Expect(env).To(HaveKeyWithValue("GOFLAGS", "-mod=mod"))
		Expect(env).To(HaveKeyWithValue("MILL_SPEC_LITERAL", "set-by-the-step"))
		Expect(env).NotTo(HaveKey("MILL_SPEC_ABSENT"))
	})

	// The workspace's way out — docs/sandbox.md §Network, §Credentials —
	// is set on the sidecar by the adapter as the workspace decided it, and
	// the sidecar copies it past the step's carve-out to the session's
	// tools. What the host exports about the model is another matter and
	// stops at the adapter's deny list; here the sidecar is handed it
	// directly, to prove that this boundary does not pass it either.
	decided := map[string]string{
		"HTTPS_PROXY":          "http://10.99.14.1:3128",
		"HTTP_PROXY":           "http://10.99.14.1:3128",
		"NO_PROXY":             "10.99.14.1",
		"ANTHROPIC_BASE_URL":   "http://10.99.14.1:8081",
		"ANTHROPIC_AUTH_TOKEN": "step-token-01JB",
		"ENABLE_TOOL_SEARCH":   "true",
	}

	// probed is what a session's `env` printed, through the fake harness's
	// env-probe scenario, given the sidecar's own environment.
	probed := func(env map[string]string) string {
		GinkgoHelper()
		spawned := map[string]string{
			"MILL_FAKE_SCENARIO": "env-probe",
			// A short PATH keeps the probe's output inside the limit the
			// sidecar truncates a tool result at.
			"PATH": "/usr/bin:/bin",
		}
		for name, value := range env {
			spawned[name] = value
		}
		s := run(fakeEntrypoint, spawned, startRequest("env-3", func(start *harnessv1.Start) {
			start.Workspace = &harnessv1.Workspace{Socket: "/run/mill/workspace-01JB.sock", Token: "step-token-01JB"}
			start.Settings = settings(map[string]any{"env": map[string]any{
				"set": map[string]any{"HTTPS_PROXY": "http://the-steps-own-proxy.test:3128"},
			}})
		}))
		printed := ""
		for _, event := range s.events() {
			if result := event.GetToolResult(); result != nil {
				printed += result.GetContent()
			}
		}
		return printed
	}

	It("The workspace's network decisions reach the session", func() {
		By("given a sidecar spawned with the proxy and the gateway the workspace decided")
		By("when the session runs `env` through its exec tool")
		printed := probed(decided)

		By("then HTTPS_PROXY, HTTP_PROXY, NO_PROXY, ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN are present, and ENABLE_TOOL_SEARCH beside the gateway")
		for name, value := range decided {
			Expect(printed).To(ContainSubstring(name + "=" + value + "\n"))
		}

		By("and the step's own carve-out did not choose its way out")
		Expect(printed).NotTo(ContainSubstring("the-steps-own-proxy"))
	})

	It("The host's model credentials still do not", func() {
		By("given CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY on the sidecar's own process")
		env := map[string]string{
			"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-should-not-leak",
			"ANTHROPIC_API_KEY":       "sk-ant-should-not-leak",
		}
		for name, value := range decided {
			env[name] = value
		}

		By("when the session runs `env` through its exec tool")
		printed := probed(env)

		By("then neither is in the session's environment, and the workspace's five are")
		Expect(printed).NotTo(ContainSubstring("CLAUDE_CODE_OAUTH_TOKEN"))
		Expect(printed).NotTo(ContainSubstring("ANTHROPIC_API_KEY"))
		Expect(printed).NotTo(ContainSubstring("should-not-leak"))
		for name, value := range decided {
			Expect(printed).To(ContainSubstring(name + "=" + value + "\n"))
		}
	})

	It("The workspace's dead sockets reach the session, not the production ones", func() {
		By("given a sidecar spawned with the dead DG_SOCKET and MILL_SOCKET the workspace decided")
		env := map[string]string{
			"DG_SOCKET":   "/var/lib/mill/runs/01JB/no-dagger.sock",
			"MILL_SOCKET": "/var/lib/mill/runs/01JB/no-mill.sock",
		}
		for name, value := range decided {
			env[name] = value
		}

		By("when the session runs `env` through its exec tool")
		printed := probed(env)

		By("then DG_SOCKET and MILL_SOCKET are the workspace's dead paths")
		Expect(printed).To(ContainSubstring("DG_SOCKET=/var/lib/mill/runs/01JB/no-dagger.sock\n"))
		Expect(printed).To(ContainSubstring("MILL_SOCKET=/var/lib/mill/runs/01JB/no-mill.sock\n"))

		By("and neither the host's dagger nor the host's mill socket ever appears")
		Expect(printed).NotTo(ContainSubstring("/run/dagger/dg.sock"))
		Expect(printed).NotTo(ContainSubstring("/run/mill/mill.sock"))
	})

	It("gives the session the workspace's git identity, which the step cannot set aside", func() {
		By("given a step that names an identity of its own")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("env-2", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"env": map[string]any{
				"set": map[string]any{"GIT_AUTHOR_EMAIL": "somebody@elsewhere.test"},
			}})
		})

		By("when the sidecar starts the session, carrying what the workspace decided")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
			"GIT_AUTHOR_EMAIL":   "mill@example.test",
			"GIT_CONFIG_GLOBAL":  "/run/mill/01J0RUN/gitconfig",
		}, line)

		By("then the session's git is the workspace's, not the step's")
		env, ok := applied(options)["env"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no environment")
		Expect(env).To(HaveKeyWithValue("GIT_AUTHOR_EMAIL", "mill@example.test"))
		Expect(env).To(HaveKeyWithValue("GIT_CONFIG_GLOBAL", "/run/mill/01J0RUN/gitconfig"))
	})

	It("refuses an option whose type is not the one the harness has", func() {
		By("given a step whose tool lists are a string rather than a list")
		line := startRequest("policy-4", func(start *harnessv1.Start) {
			start.Settings = settings(map[string]any{"tools": map[string]any{"deny": "Write"}})
		})

		By("when the sidecar runs")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, line)

		By("then the session never ran, and the reason names the key")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_POLICY))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("tools.deny"))
	})

	It("applies the structured-output schema and carries output that validates against it", func() {
		By("given a step that declares a reviewer's output schema")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		line := startRequest("structured-1", func(start *harnessv1.Start) {
			start.OutputSchema = reviewSchema
		})

		By("when the session ends")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "structured",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the schema reached the harness as its output format")
		applied := map[string]any{}
		body, err := os.ReadFile(options)
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(body, &applied)).To(Succeed())
		format, ok := applied["outputFormat"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no output format")
		Expect(format["type"]).To(Equal("json_schema"))
		declared := map[string]any{}
		Expect(json.Unmarshal([]byte(reviewSchema), &declared)).To(Succeed())
		Expect(format["schema"]).To(Equal(declared))

		By("and the terminal record's structured output validates against the schema")
		output := structuredOutput(s.terminal())
		Expect(output).To(HaveKey("decision"))
		Expect(output).To(HaveKey("findings"))
		Expect(output).To(HaveKey("summary"))
		Expect(output["decision"]).To(BeElementOf("approve", "revise", "needs_human"))
		Expect(output["findings"]).To(BeAssignableToTypeOf([]any{}))
		summary, ok := output["summary"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(summary["headline"]).To(BeAssignableToTypeOf(""))
		Expect(summary["confidence"]).To(BeAssignableToTypeOf(float64(0)))
	})

	It("fails a step whose session produced nothing matching the declared schema", func() {
		By("given a step that declares a schema and a turn that answers without one")
		line := startRequest("structured-2", func(start *harnessv1.Start) {
			start.OutputSchema = reviewSchema
		})

		By("when the session ends")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "structured-missing"}, line)

		By("then the step ran and failed, rather than passing with no output")
		Expect(s.terminal().GetStructuredOutput()).To(BeNil())
		Expect(s.terminal().GetExit().GetStatus()).To(Equal(int32(1)))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED))
	})

	It("refuses a start that carries no timeout, before the harness is loaded", func() {
		By("given a Start request whose step has no budget")
		line := startRequest("no-budget", func(start *harnessv1.Start) {
			start.TimeoutMs = 0
		})

		By("when the sidecar reads it")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, line)

		By("then it is refused in a terminal record that says so")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("no timeout"))
		Expect(s.events()).To(BeEmpty())
		Expect(s.exitCode()).To(Equal(0))
	})

	It("answers a malformed frame with a refusal and no stack trace", func() {
		By("given stdin that is not a harness.v1 request")
		s := run(entrypoint, nil, "this is not a request")

		By("then the sidecar refused it in a terminal record")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
		Expect(s.terminal().GetFinalMessage()).NotTo(BeEmpty())
		Expect(s.exitCode()).To(Equal(0))

		By("and said so on the wire, not as a stack trace on stderr")
		Expect(s.stderr.String()).NotTo(ContainSubstring("at "))
		Expect(s.stderr.String()).To(BeEmpty())
	})

	It("reports a cancelled session when SIGTERM arrives during a tool call", func() {
		By("given a session that is waiting on a tool call")
		s := start(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO":    "hang",
			"MILL_FAKE_ABORT":       "throw",
			"MILL_SIDECAR_GRACE_MS": "2000",
		}, startRequest("cancel-1", nil))
		Eventually(s.events, 30*time.Second).Should(ContainElement(HaveField("GetToolCall().GetName()", "Bash")))

		By("when the worker cancels the step")
		s.terminate()
		s.drain(10 * time.Second)

		By("then the terminal record says the session never ran to a result, because it was cancelled")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_CANCELLED))
		Expect(s.exitCode()).To(Equal(0))

		By("and the tokens the cancelled turn had already streamed are still reported")
		Expect(s.terminal().GetUsage().GetByModel()).To(HaveKey("claude-opus-5"))
	})

	It("reports the cancellation itself when the harness does not stop within the grace window", func() {
		By("given a harness that ignores the abort")
		s := start(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO":    "hang",
			"MILL_FAKE_ABORT":       "ignore",
			"MILL_SIDECAR_GRACE_MS": "500",
		}, startRequest("cancel-2", nil))
		Eventually(s.events, 30*time.Second).Should(ContainElement(HaveField("GetToolCall().GetName()", "Bash")))

		By("when the worker cancels the step")
		s.terminate()

		By("then the sidecar still answers within the grace window, and exits")
		s.drain(10 * time.Second)
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_CANCELLED))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("grace window"))
		Expect(s.exitCode()).To(Equal(0))
	})

	It("The sidecar holds no credential but its environment's two values", func() {
		By("given a Start request whose env is {WORKSPACE_SOCKET, STEP_TOKEN}")
		line := startRequest("env-1", func(start *harnessv1.Start) {
			start.Workspace = &harnessv1.Workspace{
				Socket: "/run/mill/workspace-01JB.sock",
				Token:  "step-token-01JB",
			}
		})

		By("when the session runs `env` through its exec tool")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "env-probe",
			// The sidecar's own process is given a credential of each kind
			// the mill holds, so the spec proves they stop at the sidecar
			// rather than that the host happened not to have them.
			"ANTHROPIC_API_KEY": "sk-ant-should-not-leak",
			"GITHUB_TOKEN":      "ghp-should-not-leak",
			"GH_TOKEN":          "ghp-should-not-leak",
			"DAGGER_SOCKET":     "/run/dagger.sock",
			// A short PATH keeps the probe's output inside the limit the
			// sidecar truncates a tool result at; this host's own PATH is
			// longer than a tool result is allowed to be.
			"PATH": "/usr/bin:/bin",
		}, line)

		By("then the output contains those two names and no model key or forge variable")
		printed := ""
		for _, event := range s.events() {
			if result := event.GetToolResult(); result != nil {
				printed += result.GetContent()
			}
		}
		Expect(printed).To(ContainSubstring("WORKSPACE_SOCKET=/run/mill/workspace-01JB.sock"))
		Expect(printed).To(ContainSubstring("STEP_TOKEN=step-token-01JB"))
		// The key, not the prefix: ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN
		// are the way out the workspace decided, and session.ts copies them
		// past both floors on purpose (WORKSPACE_EGRESS_ENVIRONMENT,
		// docs/sandbox.md §Credentials) — "The workspace's network decisions
		// reach the session" above requires them. A sidecar whose own process
		// has them, which is every sidecar once mill runs inside its own
		// machine, would fail a blanket match on something it is meant to do.
		Expect(printed).NotTo(ContainSubstring("ANTHROPIC_API_KEY"))
		Expect(printed).NotTo(ContainSubstring("GITHUB_TOKEN"))
		Expect(printed).NotTo(ContainSubstring("GH_TOKEN"))
		Expect(printed).NotTo(ContainSubstring("DAGGER"))
		Expect(printed).NotTo(ContainSubstring("should-not-leak"))
	})

	It("Never-ran is distinct from failed", func() {
		By("given a Start request naming a session id the SDK rejects as malformed")
		line := startRequest("never-ran-1", func(start *harnessv1.Start) {
			start.SessionId = "not-a-uuid"
		})

		By("when the sidecar runs")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "bad-session-id"}, line)

		By("then Terminal.exit.status is -1 and reason is refused")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("Invalid session ID"))

		By("and a session that did run and then failed is not reported that way")
		failed := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "failed"}, startRequest("failed-1", nil))
		Expect(failed.terminal().GetExit().GetStatus()).To(Equal(int32(1)))
		Expect(failed.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED))

		By("and a session whose first SDK message is already its result is never-ran, not a clean success")
		bare := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "bare-result"}, startRequest("bare-result-1", nil))
		Expect(bare.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(bare.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
		Expect(bare.terminal().GetFinalMessage()).NotTo(Equal("Looks like nothing to do here."))
	})

	It("marks a turn every request of which the upstream refused, apart from an ordinary failed turn", func() {
		By("given a turn whose every request the upstream answered 529")
		refused := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "upstream-refused"}, startRequest("upstream-1", nil))

		By("then the session ran, and its exit reason says the upstream refused it")
		Expect(refused.terminal().GetExit().GetStatus()).To(Equal(int32(1)))
		Expect(refused.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))

		By("and a request that got no response at all counts the same way")
		unreachable := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "upstream-unreachable"}, startRequest("upstream-2", nil))
		Expect(unreachable.terminal().GetExit().GetStatus()).To(Equal(int32(1)))
		Expect(unreachable.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))

		By("and an ordinary failed turn, carrying no api_error_status, is not reclassified")
		failed := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "failed"}, startRequest("upstream-3", nil))
		Expect(failed.terminal().GetExit().GetStatus()).To(Equal(int32(1)))
		Expect(failed.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED))
	})

	It("refuses a cancel on stdin, because cancellation is a signal", func() {
		By("given a Cancel request on stdin")
		line := request(&harnessv1.Request{
			Id:   "cancel-3",
			Body: &harnessv1.Request_Cancel{Cancel: &harnessv1.Cancel{SessionId: sessionSeed}},
		})

		By("when the sidecar runs")
		s := run(entrypoint, nil, line)

		By("then it refuses, naming the signal")
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
		Expect(strings.ToUpper(s.terminal().GetFinalMessage())).To(ContainSubstring("SIGTERM"))
	})
})

// fixturePath is the fake stdio MCP server every mcp-catalog spec below
// mounts, absolute so it resolves the same regardless of the cwd a mounted
// server's process is wrapped to run under (session.ts's cwd-wrap).
func fixturePath() string {
	GinkgoHelper()

	path, err := filepath.Abs(filepath.Join("spec", "fixtures", "fake-mcp-server.mjs"))
	Expect(err).NotTo(HaveOccurred())
	return path
}

var _ = Describe("A session's mounted MCP servers", Label("unit"), func() {
	It("mounts a server the step names, resolving ${MCP_DIR} in its args and env, and allows its tools", func() {
		By("given a host catalog with one server, a browser directory, and a step naming it")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		mcpDir := GinkgoT().TempDir()
		line := startRequest("mcp-1", func(start *harnessv1.Start) {
			start.McpServers = map[string]*harnessv1.McpServer{
				"fake": {
					Command: "node",
					Args:    []string{fixturePath(), "${MCP_DIR}/arg"},
					Env:     map[string]string{"OUTPUT_DIR": "${MCP_DIR}/shots"},
				},
			}
			start.McpDir = mcpDir
			start.Settings = settings(map[string]any{"mcp": []any{"fake"}})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the harness was given a stdio server wrapped to run in the browser directory, with the token resolved")
		applied := applied(options)
		mounted, ok := applied["mcpServers"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no mcpServers")
		fake, ok := mounted["fake"].(map[string]any)
		Expect(ok).To(BeTrue())
		args, ok := fake["args"].([]any)
		Expect(ok).To(BeTrue())
		Expect(args).To(ContainElement(mcpDir))
		Expect(args).To(ContainElement(mcpDir + "/arg"))
		env, ok := fake["env"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(env).To(HaveKeyWithValue("OUTPUT_DIR", mcpDir+"/shots"))

		By("and mcp__fake__* joined the allow list")
		Expect(applied["allowedTools"]).To(ContainElement("mcp__fake__*"))
	})

	It("still mounts a server tools.deny names, but removes its tools from the allow list", func() {
		By("given a server marked global, and a step that denies its tools")
		options := filepath.Join(GinkgoT().TempDir(), "options.json")
		mcpDir := GinkgoT().TempDir()
		line := startRequest("mcp-2", func(start *harnessv1.Start) {
			start.McpServers = map[string]*harnessv1.McpServer{
				"fake": {Command: "node", Args: []string{fixturePath()}, Global: true},
			}
			start.McpDir = mcpDir
			start.Settings = settings(map[string]any{"tools": map[string]any{"deny": []any{"mcp__fake__*"}}})
		})

		By("when the sidecar starts the session")
		run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "trivial",
			"MILL_FAKE_OPTIONS":  options,
		}, line)

		By("then the server is still in the mounted set, but its tools are out of the allow list and into the deny list")
		applied := applied(options)
		mounted, ok := applied["mcpServers"].(map[string]any)
		Expect(ok).To(BeTrue(), "the harness was given no mcpServers")
		Expect(mounted).To(HaveKey("fake"))
		Expect(applied["disallowedTools"]).To(ContainElement("mcp__fake__*"))
		Expect(applied["allowedTools"]).NotTo(ContainElement("mcp__fake__*"))
	})

	It("refuses a step whose mcp names a server neither the worker nor the workflow declares", func() {
		By("given a step naming a server absent from the catalog")
		mcpDir := GinkgoT().TempDir()
		line := startRequest("mcp-3", func(start *harnessv1.Start) {
			start.McpDir = mcpDir
			start.Settings = settings(map[string]any{"mcp": []any{"nope"}})
		})

		By("when the sidecar runs")
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "trivial"}, line)

		By("then the session never ran, and the reason names the server")
		Expect(s.terminal().GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(s.terminal().GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_POLICY))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("nope"))
		Expect(s.events()).To(BeEmpty())
	})

	It("spawns the mounted server for real, forwards a tool call to it, and leaves no process behind", func() {
		By("given a host catalog with one real fixture server, mounted globally")
		pidFile := filepath.Join(GinkgoT().TempDir(), "mcp.pid")
		mcpDir := GinkgoT().TempDir()
		line := startRequest("mcp-4", func(start *harnessv1.Start) {
			start.McpServers = map[string]*harnessv1.McpServer{
				"fake": {Command: "node", Args: []string{fixturePath()}, Global: true},
			}
			start.McpDir = mcpDir
		})

		By("when the session runs, mounting and calling the fixture for real")
		s := run(fakeEntrypoint, map[string]string{
			"MILL_FAKE_SCENARIO": "mcp-mount",
			pidVar:               pidFile,
		}, line)

		By("then the forwarded call appears on the wire as an ordinary tool call and result")
		Expect(s.events()).To(ContainElement(HaveField("GetToolCall().GetName()", "mcp__fake__fake_tool")))
		checked := results(s)
		Expect(checked).To(HaveKey("toolu_mcp"))
		Expect(checked["toolu_mcp"]).To(ContainSubstring("ping"))

		By("and the server's process is gone once the step ends")
		pid := spawnedPid(pidFile)
		Eventually(func() bool { return alive(pid) }, 10*time.Second).Should(BeFalse())
	})
})
