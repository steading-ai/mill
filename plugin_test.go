package mill_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// The Claude Code plugin: the `.mcp.json` that starts `mill mcp` and the
// skill that teaches a session what its tools answer (docs/prd.md R-56).
// What is asserted here is the plugin against the binary it documents — a
// reference that names a tool the server does not register, or misses one it
// does, is a session told the wrong thing.

const (
	pluginDir       = "plugin/mill"
	pluginMCPConfig = pluginDir + "/.mcp.json"
	pluginSkill     = pluginDir + "/skills/mill/SKILL.md"
	pluginReference = pluginDir + "/skills/mill/references/tools.md"
)

// referenceEntry matches a tool's section in the reference document. Each
// tool is a `## <name>` heading and nothing else is spelled that way: prose
// headings are capitalised, tool names are lowercase snake_case.
var referenceEntry = regexp.MustCompile(`(?m)^## ([a-z][a-z_]*)$`)

// describedTool is one entry of what `mill mcp --describe` prints, which is
// what `tools/list` answers.
type describedTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// millBinary builds the binary once for the specs that drive it.
func millBinary() string {
	GinkgoHelper()
	path, err := gexec.Build("github.com/ivy/mill/cmd/mill")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(gexec.CleanupBuildArtifacts)
	return path
}

// describedTools runs `mill mcp --describe` and decodes it. It connects to
// nothing, so it answers whether or not a service is running.
func describedTools(binary string) []describedTool {
	GinkgoHelper()
	session, err := gexec.Start(exec.Command(binary, "mcp", "--describe"), GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Expect(session.Wait(30 * time.Second)).To(gexec.Exit(0))

	var described struct {
		Tools []describedTool `json:"tools"`
	}
	Expect(json.Unmarshal(session.Out.Contents(), &described)).To(Succeed())
	return described.Tools
}

// registration is one server of the plugin's `.mcp.json`: the command a
// session spawns and the arguments it passes.
type registration struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// pluginServers is what the plugin registers.
func pluginServers() map[string]registration {
	GinkgoHelper()
	body, err := os.ReadFile(pluginMCPConfig)
	Expect(err).NotTo(HaveOccurred())
	var config struct {
		MCPServers map[string]registration `json:"mcpServers"`
	}
	Expect(json.Unmarshal(body, &config)).To(Succeed())
	return config.MCPServers
}

// pluginSession is the plugin's server as a session drives it: the command
// the `.mcp.json` names, spoken to over stdio, one JSON-RPC message per line.
type pluginSession struct {
	requests io.WriteCloser
	replies  *bufio.Reader
	next     int
}

// startPluginSession spawns exactly what the plugin registers. The binary
// built from this tree stands in for an installed `mill`, so the spec
// exercises the registration rather than whatever happens to be on PATH.
func startPluginSession(binary string, server registration) *pluginSession {
	GinkgoHelper()
	bin := GinkgoT().TempDir()
	Expect(os.Symlink(binary, filepath.Join(bin, "mill"))).To(Succeed())

	command := server.Command
	if !filepath.IsAbs(command) {
		command = filepath.Join(bin, command)
	}
	cmd := exec.Command(command, server.Args...) //nolint:gosec // the command is this repository's own plugin manifest
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdin, err := cmd.StdinPipe()
	Expect(err).NotTo(HaveOccurred())
	stdout, err := cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Start()).To(Succeed())
	DeferCleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return &pluginSession{requests: stdin, replies: bufio.NewReader(stdout)}
}

// call sends one request and returns its result.
func (s *pluginSession) call(method string, params map[string]any) map[string]any {
	GinkgoHelper()
	s.next++
	request := map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method}
	if params != nil {
		request["params"] = params
	}
	raw, err := json.Marshal(request)
	Expect(err).NotTo(HaveOccurred())
	_, err = s.requests.Write(append(raw, '\n'))
	Expect(err).NotTo(HaveOccurred())

	line, err := s.replies.ReadBytes('\n')
	Expect(err).NotTo(HaveOccurred())
	var response struct {
		Result map[string]any  `json:"result"`
		Error  *map[string]any `json:"error"`
	}
	Expect(json.Unmarshal(line, &response)).To(Succeed())
	Expect(response.Error).To(BeNil(), string(line))
	return response.Result
}

// tool calls one tool and decodes the JSON it answered with.
func (s *pluginSession) tool(name string, args map[string]any) map[string]any {
	GinkgoHelper()
	result := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	content, ok := result["content"].([]any)
	Expect(ok).To(BeTrue(), "%v", result)
	Expect(content).To(HaveLen(1))
	text, ok := content[0].(map[string]any)["text"].(string)
	Expect(ok).To(BeTrue())
	Expect(result["isError"]).To(BeNil(), text)

	var decoded map[string]any
	Expect(json.Unmarshal([]byte(text), &decoded)).To(Succeed(), text)
	return decoded
}

var _ = Describe("The mill plugin", Label("unit", "slow"), func() {
	It("The reference matches the server", func() {
		By("given the built binary")
		tools := describedTools(millBinary())
		Expect(tools).NotTo(BeEmpty())

		By("when mill mcp --describe is compared with the reference document")
		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			Expect(tool.Description).NotTo(BeEmpty(), tool.Name)
			Expect(tool.InputSchema).To(HaveKeyWithValue("type", "object"), tool.Name)
			names = append(names, tool.Name)
		}
		body, err := os.ReadFile(pluginReference)
		Expect(err).NotTo(HaveOccurred())
		documented := make([]string, 0, len(names))
		for _, match := range referenceEntry.FindAllStringSubmatch(string(body), -1) {
			documented = append(documented, match[1])
		}

		By("then every tool appears in both with the same name")
		Expect(documented).To(ConsistOf(names))
	})

	It("registers mill mcp as its only server", func() {
		servers := pluginServers()
		Expect(servers).To(HaveLen(1))
		Expect(servers["mill"].Command).To(Equal("mill"))
		Expect(servers["mill"].Args).To(Equal([]string{"mcp"}))
	})

	It("states the role rules and the file-a-node rule in the skill's first screen", func() {
		body, err := os.ReadFile(pluginSkill)
		Expect(err).NotTo(HaveOccurred())
		// The frontmatter is not the skill's prose; the first screen is the
		// first fifty lines of what a reader sees after it.
		_, prose, found := splitFrontmatter(string(body))
		Expect(found).To(BeTrue(), "the skill needs frontmatter to be loaded at all")
		firstScreen := head(prose, 50)

		Expect(firstScreen).To(ContainSubstring("File a dagger node"))
		Expect(firstScreen).To(ContainSubstring("`run` is not an MCP tool"))
		for _, role := range []string{"`operator`", "`reader`"} {
			Expect(firstScreen).To(ContainSubstring(role))
		}
		for _, tool := range []string{"`cancel`", "`rerun`", "`workspace_clean`"} {
			Expect(firstScreen).To(ContainSubstring(tool))
		}
	})

	// The service side of the plugin. It needs a mill answering on a socket,
	// which a unit run has no business starting, so it runs only where the
	// operator points it at one.
	//
	// What is driven is the registration itself — the command and arguments
	// the `.mcp.json` names, spoken to over stdio the way a session speaks
	// to it — rather than a `claude` process loading the plugin. A spec that
	// launched a session would bill an account and assert about a model's
	// choices; this one asserts about the server the session would reach.
	It("A session can read a run", func() {
		By("given a running mill and a session with the plugin")
		if os.Getenv("MILL_SOCKET") == "" {
			Skip("MILL_SOCKET is unset: no running mill for a session to read")
		}
		s := startPluginSession(millBinary(), pluginServers()["mill"])
		Expect(s.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})).
			To(HaveKeyWithValue("protocolVersion", "2025-06-18"))

		listed := s.call("tools/list", nil)
		names := []string{}
		for _, tool := range listed["tools"].([]any) {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
		Expect(names).To(ContainElements("status", "inspect", "explain", "logs"))

		live := s.tool("status", map[string]any{"view": "refs", "statuses": []string{}})
		runs, ok := live["runs"].([]any)
		Expect(ok).To(BeTrue(), "%v", live)
		if len(runs) == 0 {
			Skip("the ledger on " + os.Getenv("MILL_SOCKET") + " holds no run to inspect")
		}
		reference := runs[0].(map[string]any)["run"].(string)

		By("when the session calls inspect for a run id")
		answer := s.tool("inspect", map[string]any{"run": reference})

		By("then it receives the run's steps and evidence")
		Expect(answer).To(HaveKeyWithValue("run", reference))
		steps, ok := answer["steps"].([]any)
		Expect(ok).To(BeTrue(), "%v", answer)
		for _, step := range steps {
			Expect(step).To(HaveKey("step"))
			Expect(step).To(HaveKey("attempt"))
			Expect(step).To(HaveKey("evidence_kind"))
			Expect(step).To(HaveKey("evidence"))
		}
	})
})

// splitFrontmatter separates a skill's YAML frontmatter from its prose.
func splitFrontmatter(body string) (frontmatter, prose string, found bool) {
	const fence = "---\n"
	if !strings.HasPrefix(body, fence) {
		return "", body, false
	}
	rest := body[len(fence):]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return "", body, false
	}
	return rest[:end], rest[end+len("\n"+fence):], true
}

// head is the first n lines of a document: what a reader sees before
// scrolling.
func head(body string, n int) string {
	lines := strings.Split(body, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
