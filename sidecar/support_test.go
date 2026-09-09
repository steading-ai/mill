package sidecar_test

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
)

const (
	// entrypoint is the path the worker spawns: the sidecar wired to the
	// Claude Agent SDK.
	entrypoint = "dist/main.js"
	// fakeEntrypoint is the same sidecar wired to the fake harness in
	// spec/fake_sdk.ts. Only the harness differs.
	fakeEntrypoint = "dist/spec/fake_main.js"
	// liveVar turns on the specs that spend a real session against the
	// account's own Claude Code login.
	liveVar = "MILL_SIDECAR_LIVE"
	// sessionSeed is a well-formed session id for specs that need one and do
	// not care which.
	sessionSeed = "0f2f4d3a-6d6f-4d2b-9e2c-0d5b7a1c4e11"
)

// settings is an options document as a step carries it, for the specs that
// give the sidecar one. The document's schema is the sidecar's own, in
// src/options.ts; nothing on this side of the wire interprets it.
func settings(document map[string]any) *harnessv1.Settings {
	GinkgoHelper()
	options, err := structpb.NewStruct(document)
	Expect(err).NotTo(HaveOccurred())
	return &harnessv1.Settings{Options: options}
}

// build compiles the sidecar. The specs run what the worker runs — compiled
// JavaScript under dist/ — rather than a type-stripped shortcut, because the
// generated wire types are part of what is under test. It runs once for the
// whole suite, from the suite bootstrap: parallel processes sharing one dist
// must not compile into it at the same time.
func build() {
	GinkgoHelper()

	if _, err := os.Stat("node_modules"); os.IsNotExist(err) {
		pnpm("install", "--frozen-lockfile")
	}
	pnpm("run", "build")
}

func pnpm(args ...string) {
	GinkgoHelper()

	cmd := exec.Command("pnpm", args...)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed(), "pnpm %v", args)
}

// live skips a spec unless the environment asks for a real session.
func live() {
	GinkgoHelper()

	if os.Getenv(liveVar) == "" {
		Skip("set " + liveVar + "=1 to run the specs that spend a real session")
	}
}

// sidecar is one spawned sidecar, with its stdout parsed into responses as
// they arrive.
type sidecar struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *strings.Builder
	waited error

	// closed when the sidecar's stdout reaches end of file.
	done chan struct{}

	mu        sync.Mutex
	collected []*harnessv1.Response
}

// start spawns the sidecar and writes one request line to its stdin, leaving
// stdin open: a worker holds the pipe for the life of the step, and a
// sidecar that read EOF as a reason to stop would never see a signal.
func start(entry string, env map[string]string, request string) *sidecar {
	GinkgoHelper()

	cmd := exec.Command("node", entry)
	cmd.Env = append(os.Environ(), asPairs(env)...)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	Expect(err).NotTo(HaveOccurred())
	stdout, err := cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	Expect(cmd.Start()).To(Succeed())

	s := &sidecar{cmd: cmd, stdin: stdin, stderr: stderr, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			response := &harnessv1.Response{}
			if err := protojson.Unmarshal(line, response); err != nil {
				// A line the wire cannot parse is a failure of the thing
				// under test; the spec sees it as a missing response and the
				// raw line lands in the report.
				GinkgoWriter.Printf("unparseable line: %s: %v\n", line, err)
				continue
			}
			s.mu.Lock()
			s.collected = append(s.collected, response)
			s.mu.Unlock()
		}
	}()

	_, err = io.WriteString(stdin, request+"\n")
	Expect(err).NotTo(HaveOccurred())
	return s
}

func asPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for name, value := range env {
		pairs = append(pairs, name+"="+value)
	}
	return pairs
}

// responses is what the sidecar has written so far, safe to read while it is
// still running.
func (s *sidecar) responses() []*harnessv1.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*harnessv1.Response{}, s.collected...)
}

// events are every event the sidecar has written so far.
func (s *sidecar) events() []*harnessv1.Event {
	events := []*harnessv1.Event{}
	for _, response := range s.responses() {
		if event := response.GetEvent(); event != nil {
			events = append(events, event)
		}
	}
	return events
}

// drain reads until the sidecar closes its stdout and exits, and returns
// everything it wrote.
func (s *sidecar) drain(timeout time.Duration) *sidecar {
	GinkgoHelper()

	Eventually(s.done, timeout).Should(BeClosed(), "the sidecar did not close its stdout: %s", s.stderr)
	s.waited = s.cmd.Wait()
	return s
}

// terminals are the terminal records the sidecar wrote. Exactly one is the
// contract, so specs assert on the count as well as the content.
func (s *sidecar) terminals() []*harnessv1.Terminal {
	terminals := []*harnessv1.Terminal{}
	for _, response := range s.responses() {
		if terminal := response.GetTerminal(); terminal != nil {
			terminals = append(terminals, terminal)
		}
	}
	return terminals
}

// terminal is the sidecar's single terminal record.
func (s *sidecar) terminal() *harnessv1.Terminal {
	GinkgoHelper()

	terminals := s.terminals()
	Expect(terminals).To(HaveLen(1), "expected exactly one terminal record")
	return terminals[0]
}

// exitCode is the status the sidecar process exited with.
func (s *sidecar) exitCode() int {
	GinkgoHelper()

	if s.waited == nil {
		return 0
	}
	exitErr := &exec.ExitError{}
	Expect(s.waited).To(BeAssignableToTypeOf(exitErr), "the sidecar failed to run: %v", s.waited)
	return s.waited.(*exec.ExitError).ExitCode()
}

// terminate sends the signal a worker cancels a step with.
func (s *sidecar) terminate() {
	GinkgoHelper()

	Expect(s.cmd.Process.Signal(syscall.SIGTERM)).To(Succeed())
}

// run spawns the sidecar, closes its stdin, and waits for it to finish.
func run(entry string, env map[string]string, request string) *sidecar {
	GinkgoHelper()

	return runFor(entry, env, request, 90*time.Second)
}

// runFor is run with a deadline of its caller's choosing: a real session
// takes as long as the model does.
func runFor(entry string, env map[string]string, request string, timeout time.Duration) *sidecar {
	GinkgoHelper()

	s := start(entry, env, request)
	Expect(s.stdin.Close()).To(Succeed())
	return s.drain(timeout)
}

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

// request renders a harness.v1 Request as the worker sends it: one line of
// protojson.
func request(message *harnessv1.Request) string {
	GinkgoHelper()

	line, err := protojson.Marshal(message)
	Expect(err).NotTo(HaveOccurred())
	return string(line)
}

// structuredOutput renders a terminal's structured output as ordinary Go
// values, which is the shape a schema is checked against.
func structuredOutput(terminal *harnessv1.Terminal) map[string]any {
	GinkgoHelper()

	Expect(terminal.GetStructuredOutput()).NotTo(BeNil(), "the terminal record carries no structured output")
	encoded, err := terminal.GetStructuredOutput().MarshalJSON()
	Expect(err).NotTo(HaveOccurred())
	decoded := map[string]any{}
	Expect(json.Unmarshal(encoded, &decoded)).To(Succeed())
	return decoded
}
