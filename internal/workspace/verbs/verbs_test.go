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

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/signing"
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
	// No identity leaks in from the developer's own git configuration: a
	// production checkout has none, and git finds ~/.gitconfig through the
	// account even with no HOME in the environment.
	env := map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull}
	for k, v := range command.Env {
		env[k] = v
	}
	command.Env = env
	return workspace.Spawner{Floor: []string{}}.Exec(ctx, command)
}

// fakeSink is a [workspace.Sink] that keeps every call it was handed, in
// order, for a spec to read back.
type fakeSink struct {
	mu      sync.Mutex
	started []sunk
	output  []sunk
	ended   []sunk
}

type sunk struct {
	verb, stream string
	verbID       int64
	data         []byte
	err          error
}

func (f *fakeSink) Started(verb string, verbID int64, _ map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, sunk{verb: verb, verbID: verbID})
}

func (f *fakeSink) Output(verb string, verbID int64, stream string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.output = append(f.output, sunk{verb: verb, verbID: verbID, stream: stream, data: data})
}

func (f *fakeSink) Ended(verb string, verbID int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, sunk{verb: verb, verbID: verbID, err: err})
}

// repository is a checkout with one commit on main.
func repository() string {
	GinkgoHelper()
	dir := filepath.Join(GinkgoT().TempDir(), "repo")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	runGit(dir, "init", "--quiet", "--initial-branch=main")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600)).To(Succeed())
	runGit(dir, "add", "README.md")
	runGit(dir, "commit", "--quiet", "--no-gpg-sign", "--message", "seed")
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

	It("answers CatchUp with ErrNoForge on a workspace with no forge, and fetches nothing", func(ctx context.Context) {
		v := verbs.Verbs{Host: host, Checkout: func() (verbs.Checkout, error) { return verbs.Checkout{Dir: dir, Branch: "main"}, nil }}
		_, err := v.CatchUp(ctx, workspace.CatchUpRequest{})
		Expect(err).To(MatchError(workspace.ErrNoForge))
		Expect(execs.commands).To(BeEmpty())
	})

	It("sinks a verb's start and its stdout, and ends it with no error, on success", func(ctx context.Context) {
		sink := &fakeSink{}
		host.Sink = sink
		out, err := host.Run(ctx, dir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		Expect(sink.started).To(HaveLen(1))
		Expect(sink.started[0].verb).To(Equal("git.rev-parse"))
		verbID := sink.started[0].verbID
		Expect(sink.output).To(Equal([]sunk{{verb: "git.rev-parse", verbID: verbID, stream: "stdout", data: []byte(out)}}),
			"no call for the empty stderr stream")
		Expect(sink.ended).To(Equal([]sunk{{verb: "git.rev-parse", verbID: verbID}}))
	})

	It("sinks a verb's stderr and ends it with the command's own error, on failure", func(ctx context.Context) {
		sink := &fakeSink{}
		host.Sink = sink
		_, err := host.Run(ctx, dir, nil, "show", "does-not-exist")
		Expect(err).To(HaveOccurred())
		Expect(sink.started).To(HaveLen(1))
		Expect(sink.started[0].verb).To(Equal("git.show"))
		verbID := sink.started[0].verbID
		Expect(sink.output).To(HaveLen(1), "no call for the empty stdout stream")
		Expect(sink.output[0].verb).To(Equal("git.show"))
		Expect(sink.output[0].verbID).To(Equal(verbID))
		Expect(sink.output[0].stream).To(Equal("stderr"))
		Expect(sink.ended).To(Equal([]sunk{{verb: "git.show", verbID: verbID, err: err}}))
	})

	It("mints a distinct verb id for each invocation on the same host", func(ctx context.Context) {
		sink := &fakeSink{}
		host.Sink = sink
		_, err := host.Run(ctx, dir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		_, err = host.Run(ctx, dir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		Expect(err).NotTo(HaveOccurred())
		Expect(sink.started).To(HaveLen(2))
		Expect(sink.started[0].verbID).NotTo(Equal(sink.started[1].verbID))
	})

	It("tells the same repository under the suffixes git ignores", func() {
		Expect(verbs.SameRepo("https://forge.test/g/r.git", "https://forge.test/g/r/")).To(BeTrue())
		Expect(verbs.SameRepo("https://forge.test/g/r", "https://forge.test/g/other")).To(BeFalse())
		Expect(verbs.SameRepo("", "https://forge.test/g/r")).To(BeFalse())
	})
})

// runGit runs one git command for a catch-up spec, with a fixed identity and
// no configuration outside the repository, mirroring [repository]'s own.
func runGit(dir string, args ...string) string {
	GinkgoHelper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir,
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_AUTHOR_NAME=spec", "GIT_AUTHOR_EMAIL=spec@example.test",
		"GIT_COMMITTER_NAME=spec", "GIT_COMMITTER_EMAIL=spec@example.test",
	}
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	return string(out)
}

// cloneOf makes a clone of origin, with the identity the clone's own commits
// need and nothing else configured, and switches it to branch — the run's
// own branch, distinct from origin's checked-out one so a push back never
// touches a branch origin has checked out.
func cloneOf(origin, branch string) string {
	GinkgoHelper()
	parent := GinkgoT().TempDir()
	dir := filepath.Join(parent, "clone")
	runGit(parent, "clone", "--quiet", origin, dir)
	runGit(dir, "config", "user.name", "spec")
	runGit(dir, "config", "user.email", "spec@example.test")
	runGit(dir, "checkout", "--quiet", "-b", branch)
	return dir
}

// catchUpForge is a [forge.Forge] whose only live verb is Push, the one
// CatchUp's self-merge uses to land back on origin; every other verb is
// merge's own and unreached from here.
type catchUpForge struct {
	mu     sync.Mutex
	pushed []string
}

func (f *catchUpForge) Authenticate(context.Context) (forge.GitAuth, error) {
	return forge.GitAuth{}, nil
}

func (f *catchUpForge) Push(ctx context.Context, request forge.PushRequest) (string, error) {
	if _, err := request.Git.Run(ctx, nil, "push", "origin", request.Branch); err != nil {
		return "", err
	}
	out, err := request.Git.Run(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.pushed = append(f.pushed, request.Branch)
	f.mu.Unlock()
	return strings.TrimSpace(out), nil
}

func (f *catchUpForge) OpenPR(context.Context, forge.OpenPRRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, errors.New("catchUpForge: OpenPR is not catch-up's to call")
}

func (f *catchUpForge) PR(context.Context, string) (forge.PullRequest, error) {
	return forge.PullRequest{}, errors.New("catchUpForge: PR is not catch-up's to call")
}

func (f *catchUpForge) Checks(context.Context, string) (forge.Status, error) {
	return "", errors.New("catchUpForge: Checks is not catch-up's to call")
}

func (f *catchUpForge) Merge(context.Context, forge.PullRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, errors.New("catchUpForge: Merge is not catch-up's to call")
}

func (f *catchUpForge) MergeCommit(context.Context, forge.PullRequest) (string, error) {
	return "", errors.New("catchUpForge: MergeCommit is not catch-up's to call")
}

func (f *catchUpForge) ClosePR(context.Context, forge.PullRequest, string) error {
	return errors.New("catchUpForge: ClosePR is not catch-up's to call")
}

func (f *catchUpForge) DeleteBranch(context.Context, string) error {
	return errors.New("catchUpForge: DeleteBranch is not catch-up's to call")
}

var _ forge.Forge = (*catchUpForge)(nil)

var _ = Describe("Catching up with a target branch", Label("unit"), func() {
	var (
		origin string
		clone  string
		host   verbs.Host
		f      *catchUpForge
	)

	BeforeEach(func() {
		origin = repository()
		clone = cloneOf(origin, "feature-x")
		host = verbs.Host{Execer: &recorder{}, Redactor: redact.New()}
		f = &catchUpForge{}
	})

	verbsOf := func() verbs.Verbs {
		return verbs.Verbs{Host: host, Forge: f, Checkout: func() (verbs.Checkout, error) {
			return verbs.Checkout{Dir: clone, Branch: "feature-x"}, nil
		}}
	}

	It("reports UpToDate and pushes nothing when the branch already has the target's head", func(ctx context.Context) {
		result, err := verbsOf().CatchUp(ctx, workspace.CatchUpRequest{Target: "main"})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.UpToDate).To(BeTrue())
		Expect(result.Merged).To(BeFalse())
		Expect(f.pushed).To(BeEmpty())
	})

	It("merges and pushes itself when the two sides touched no file in common", func(ctx context.Context) {
		Expect(os.WriteFile(filepath.Join(clone, "mine.txt"), []byte("mine\n"), 0o600)).To(Succeed())
		runGit(clone, "add", "mine.txt")
		runGit(clone, "commit", "--quiet", "--no-gpg-sign", "--message", "mine")

		Expect(os.WriteFile(filepath.Join(origin, "theirs.txt"), []byte("theirs\n"), 0o600)).To(Succeed())
		runGit(origin, "add", "theirs.txt")
		runGit(origin, "commit", "--quiet", "--no-gpg-sign", "--message", "theirs")

		result, err := verbsOf().CatchUp(ctx, workspace.CatchUpRequest{Target: "main"})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.UpToDate).To(BeFalse())
		Expect(result.Merged).To(BeTrue())
		Expect(result.Overlap).To(BeEmpty())
		Expect(result.SHA).NotTo(BeEmpty())

		Expect(f.pushed).To(Equal([]string{"feature-x"}))
		Expect(strings.TrimSpace(runGit(origin, "rev-parse", "feature-x"))).To(Equal(result.SHA),
			"the merge landed on origin under the run's own branch")
		Expect(filepath.Join(clone, "mine.txt")).To(BeAnExistingFile())
		Expect(filepath.Join(clone, "theirs.txt")).To(BeAnExistingFile())
	})

	It("leaves the merge staged with conflict markers, and pushes nothing, when both sides touched the same file", func(ctx context.Context) {
		Expect(os.WriteFile(filepath.Join(clone, "README.md"), []byte("mine\n"), 0o600)).To(Succeed())
		runGit(clone, "commit", "--quiet", "--no-gpg-sign", "--all", "--message", "mine")

		Expect(os.WriteFile(filepath.Join(origin, "README.md"), []byte("theirs\n"), 0o600)).To(Succeed())
		runGit(origin, "commit", "--quiet", "--no-gpg-sign", "--all", "--message", "theirs")

		// A production checkout carries no identity of its own: the mill's
		// is the signer's, passed per invocation. git refuses to begin even
		// a --no-commit merge without one, so the staged merge must carry
		// it the way the finished merge and the commit verb do.
		runGit(clone, "config", "--unset", "user.name")
		runGit(clone, "config", "--unset", "user.email")
		// Ask git not to guess one from the account and hostname either,
		// which it does on a developer's Mac and not on a guest whose
		// hostname has no domain: the spec must fail the same way anywhere.
		runGit(clone, "config", "user.useConfigOnly", "true")
		key, err := signing.Generate(ctx, workspace.Spawner{Floor: []string{"PATH=" + os.Getenv("PATH")}},
			GinkgoT().TempDir(), "mill", "mill@example.test")
		Expect(err).NotTo(HaveOccurred())
		signed := verbs.Verbs{Host: host, Forge: f, Checkout: func() (verbs.Checkout, error) {
			return verbs.Checkout{Dir: clone, Branch: "feature-x", Signer: key}, nil
		}}

		result, err := signed.CatchUp(ctx, workspace.CatchUpRequest{Target: "main"})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.UpToDate).To(BeFalse())
		Expect(result.Merged).To(BeFalse())
		Expect(result.Overlap).To(Equal([]string{"README.md"}))
		Expect(f.pushed).To(BeEmpty())

		content, err := os.ReadFile(filepath.Join(clone, "README.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(ContainSubstring("<<<<<<<"), "git left conflict markers for implement and commit to resolve")
	})
})
