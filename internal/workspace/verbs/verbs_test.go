package verbs_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/verbs"
)

// recorder spawns for real and keeps every command it was handed.
type recorder struct {
	mu       sync.Mutex
	commands []workspace.Command
}

func (r *recorder) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	r.mu.Lock()
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	return workspace.Spawner{Floor: []string{}}.Exec(ctx, command)
}

// fakeSink is a [workspace.Sink] that keeps every call it was handed, in
// order, for a spec to read back.
type fakeSink struct {
	mu      sync.Mutex
	started []string
	output  []sunk
	ended   []sunk
}

type sunk struct {
	verb, stream string
	data         []byte
	err          error
}

func (f *fakeSink) Started(verb string, _ map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, verb)
}

func (f *fakeSink) Output(verb, stream string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.output = append(f.output, sunk{verb: verb, stream: stream, data: data})
}

func (f *fakeSink) Ended(verb string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, sunk{verb: verb, err: err})
}

// repository is a checkout with one commit on main.
func repository() string {
	GinkgoHelper()
	dir := filepath.Join(GinkgoT().TempDir(), "repo")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	run := func(args ...string) {
		GinkgoHelper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull,
			"GIT_AUTHOR_NAME=spec", "GIT_AUTHOR_EMAIL=spec@example.test", "GIT_COMMITTER_NAME=spec", "GIT_COMMITTER_EMAIL=spec@example.test"}
		out, err := command.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	}
	run("init", "--quiet", "--initial-branch=main")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600)).To(Succeed())
	run("add", "README.md")
	run("commit", "--quiet", "--no-gpg-sign", "--message", "seed")
	return dir
}

var _ = Describe("Git on the host", Label("unit"), func() {
	var (
		dir   string
		execs *recorder
		host  verbs.Host
	)

	BeforeEach(func() {
		dir = repository()
		execs = &recorder{}
		host = verbs.Host{Execer: execs, Redactor: redact.New()}
	})

	It("names the directory in argv and as the working directory, and gives git nothing of the mill's own", func(ctx context.Context) {
		out, err := host.Run(ctx, dir, map[string]string{"EXTRA": "value"}, "rev-parse", "--abbrev-ref", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("main"))
		Expect(execs.commands).To(HaveLen(1))
		command := execs.commands[0]
		Expect(command.Path).To(Equal("git"))
		Expect(command.Args).To(Equal([]string{"-C", dir, "rev-parse", "--abbrev-ref", "HEAD"}))
		Expect(command.Dir).To(Equal(dir))
		Expect(command.Env).To(HaveKeyWithValue("GIT_CONFIG_GLOBAL", os.DevNull))
		Expect(command.Env).To(HaveKeyWithValue("GIT_CONFIG_SYSTEM", os.DevNull))
		Expect(command.Env).To(HaveKeyWithValue("GIT_TERMINAL_PROMPT", "0"))
		Expect(command.Env).To(HaveKeyWithValue("HOME", filepath.Dir(dir)), "a home outside the tree")
		Expect(command.Env).To(HaveKeyWithValue("EXTRA", "value"))
	})

	It("names the verb past -C and -c in an error, and redacts what it was handed", func(ctx context.Context) {
		_, err := host.Run(ctx, dir, map[string]string{"SECRET": "hunter2"}, "-c", "core.pager=cat", "show", "hunter2")
		Expect(err).To(MatchError(ContainSubstring("git show exited")))
		Expect(err.Error()).NotTo(ContainSubstring("hunter2"), "the value the command was handed is redacted from its stderr")
	})

	It("passes the backend's error through every verb when there is no checkout", func(ctx context.Context) {
		notCreated := errors.New("backend: not created")
		v := verbs.Verbs{Host: host, Checkout: func() (verbs.Checkout, error) { return verbs.Checkout{}, notCreated }}
		_, err := v.Git().Status(ctx)
		Expect(err).To(MatchError(notCreated))
		_, err = v.Git().Log(ctx, workspace.LogRequest{})
		Expect(err).To(MatchError(notCreated))
		_, err = v.ForgeVerbs().Checks(ctx, workspace.ChecksRequest{})
		Expect(err).To(MatchError(notCreated))
		_, err = v.Runner().Run(ctx, nil, "status")
		Expect(err).To(MatchError(notCreated))
		Expect(execs.commands).To(BeEmpty())
	})

	It("answers the forge verbs with ErrNoForge on a workspace with no forge, and pushes nothing", func(ctx context.Context) {
		v := verbs.Verbs{Host: host, Checkout: func() (verbs.Checkout, error) { return verbs.Checkout{Dir: dir, Branch: "main"}, nil }}
		_, err := v.ForgeVerbs().Push(ctx, workspace.PushRequest{})
		Expect(err).To(MatchError(workspace.ErrNoForge))
		Expect(execs.commands).To(BeEmpty())
		status, err := v.Git().Status(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(workspace.Status{Branch: "main", Clean: true}))
	})

	It("sinks a verb's start and its stdout, and ends it with no error, on success", func(ctx context.Context) {
		sink := &fakeSink{}
		host.Sink = sink
		out, err := host.Run(ctx, dir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		Expect(sink.started).To(Equal([]string{"git.rev-parse"}))
		Expect(sink.output).To(Equal([]sunk{{verb: "git.rev-parse", stream: "stdout", data: []byte(out)}}),
			"no call for the empty stderr stream")
		Expect(sink.ended).To(HaveLen(1))
		Expect(sink.ended[0].verb).To(Equal("git.rev-parse"))
		Expect(sink.ended[0].err).NotTo(HaveOccurred())
	})

	It("sinks a verb's stderr and ends it with the command's own error, on failure", func(ctx context.Context) {
		sink := &fakeSink{}
		host.Sink = sink
		_, err := host.Run(ctx, dir, nil, "show", "does-not-exist")
		Expect(err).To(HaveOccurred())
		Expect(sink.started).To(Equal([]string{"git.show"}))
		Expect(sink.output).To(HaveLen(1), "no call for the empty stdout stream")
		Expect(sink.output[0].verb).To(Equal("git.show"))
		Expect(sink.output[0].stream).To(Equal("stderr"))
		Expect(sink.ended).To(Equal([]sunk{{verb: "git.show", err: err}}))
	})

	It("tells the same repository under the suffixes git ignores", func() {
		Expect(verbs.SameRepo("https://forge.test/g/r.git", "https://forge.test/g/r/")).To(BeTrue())
		Expect(verbs.SameRepo("https://forge.test/g/r", "https://forge.test/g/other")).To(BeFalse())
		Expect(verbs.SameRepo("", "https://forge.test/g/r")).To(BeFalse())
	})
})
