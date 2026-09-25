package machine_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// gitEnv is what the specs' own git invocations run with.
func gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_AUTHOR_NAME=spec",
		"GIT_AUTHOR_EMAIL=spec@example.test",
		"GIT_COMMITTER_NAME=spec",
		"GIT_COMMITTER_EMAIL=spec@example.test",
		"LC_ALL=C",
	}
}

// git runs git in dir, failing the spec if it exits non-zero.
func git(dir string, args ...string) {
	GinkgoHelper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = gitEnv()
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
}

// origin builds a repository to clone: one commit on `main`, with the
// files given at the root.
func origin(files map[string]string) string {
	GinkgoHelper()
	dir := filepath.Join(GinkgoT().TempDir(), "origin")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	git(dir, "init", "--quiet", "--initial-branch=main")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("as cloned\n"), 0o600)).To(Succeed())
	for name, body := range files {
		Expect(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)).To(Succeed())
	}
	git(dir, "add", "--all")
	git(dir, "commit", "--quiet", "--no-gpg-sign", "--message", "seed")
	return dir
}

// gitStub is a git binary that appends its arguments and the environment it
// saw to files beside itself before running the real git: what the clone
// carried is then on record.
func gitStub(dir string) string {
	GinkgoHelper()
	path := filepath.Join(dir, "git")
	script := "#!/bin/sh\nhere=\"$(dirname \"$0\")\"\n" +
		"printf '%s\\n' \"$@\" >> \"$here/git.calls\"\n" +
		"env | sort >> \"$here/git.env\"\n" +
		"case \" $* \" in *\" clone \"*) echo clone >> \"$here/events\";; esac\n" +
		"exec /usr/bin/git \"$@\"\n"
	Expect(os.WriteFile(path, []byte(script), 0o700)).To(Succeed()) //nolint:gosec // a spec's own stub
	return path
}

// lines reads a stub's record file as lines, or nothing.
func lines(path string) []string {
	raw, err := os.ReadFile(path) //nolint:gosec // a spec's own scratch file
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// machined is the fake driver with machined's state modelled: a machine is
// registered by StartUnit and, as the host configuration says, removed by
// a stop of its unit or a terminate — unless the spec says a stop is lost,
// which is the E0 addendum's case. ShowMachine and MachineInterfaces answer
// from that state, so a lifecycle spec needs no per-call script for them.
// Everything else is the fake's, calls recorded and all.
type machined struct {
	*driver.FakeDriver
	mu       sync.Mutex
	running  map[string]bool
	lostStop bool
	events   string
}

func newMachined(events string) *machined {
	return &machined{FakeDriver: driver.NewFake(), running: map[string]bool{}, events: events}
}

// event appends one word to the shared sequence file the git stub writes
// to as well.
func (m *machined) event(word string) {
	f, err := os.OpenFile(m.events, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(word + "\n")
	_ = f.Close()
}

// machineOf is the machine a unit instance boots.
func machineOf(unit string) string {
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "mill-machine@"), ".service")
	return "mill-" + id
}

func (m *machined) StartUnit(ctx context.Context, unit string) error {
	m.event("boot")
	if err := m.FakeDriver.StartUnit(ctx, unit); err != nil {
		return err
	}
	m.mu.Lock()
	m.running[machineOf(unit)] = true
	m.mu.Unlock()
	return nil
}

func (m *machined) StopUnit(ctx context.Context, machine, unit string) error {
	if err := m.FakeDriver.StopUnit(ctx, machine, unit); err != nil {
		return err
	}
	if machine == "" && !m.lostStop {
		m.mu.Lock()
		delete(m.running, machineOf(unit))
		m.mu.Unlock()
	}
	return nil
}

func (m *machined) Terminate(ctx context.Context, machine string) error {
	if err := m.FakeDriver.Terminate(ctx, machine); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.running, machine)
	m.mu.Unlock()
	return nil
}

func (m *machined) ShowMachine(ctx context.Context, name string) (driver.Machine, error) {
	_, _ = m.FakeDriver.ShowMachine(ctx, name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running[name] {
		return driver.Machine{}, driver.ErrNoMachine
	}
	return driver.Machine{Name: name, State: driver.StateRunning, Class: "container", Service: "systemd-nspawn"}, nil
}

// ListMachines answers from machined's state, so a reconciliation spec sees
// exactly what a lifecycle spec started; a spec may still register a
// machine by hand with add, as a suite's machine or a hand-started one is.
func (m *machined) ListMachines(ctx context.Context) ([]driver.Machine, error) {
	if _, err := m.FakeDriver.ListMachines(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.running))
	for name := range m.running {
		names = append(names, name)
	}
	sort.Strings(names)
	machines := make([]driver.Machine, 0, len(names))
	for _, name := range names {
		machines = append(machines, driver.Machine{Name: name, State: driver.StateRunning, Class: "container", Service: "systemd-nspawn"})
	}
	return machines, nil
}

// add registers a machine nothing here started.
func (m *machined) add(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running[name] = true
}

func (m *machined) MachineInterfaces(ctx context.Context, name string) ([]string, error) {
	_, _ = m.FakeDriver.MachineInterfaces(ctx, name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running[name] {
		return nil, driver.ErrNoMachine
	}
	return []string{"ve-" + name[:12]}, nil
}

func (m *machined) BuildImageOnHost(ctx context.Context, build driver.ImageBuild) (string, error) {
	m.event("ensure")
	return m.FakeDriver.BuildImageOnHost(ctx, build)
}

// registered is the machines machined knows.
func (m *machined) registered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for name := range m.running {
		names = append(names, name)
	}
	return names
}

// workerForge is a forge whose credential is the worker's own: it records
// the step it is bound to, which for the clone is no step at all, and hands
// git a token through the environment the way the GitLab adapter does.
type workerForge struct {
	forge.Forge
	repo  forge.Repo
	mu    sync.Mutex
	steps []forge.Step
	mints int
}

const workerToken = "glpat-the-workers-own-credential" // gitleaks:allow

func (f *workerForge) Repo() forge.Repo { return f.repo }

func (f *workerForge) ForStep(step forge.Step) forge.Forge {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, step)
	return f
}

func (f *workerForge) Authenticate(context.Context) (forge.GitAuth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints++
	return forge.GitAuth{
		Args: []string{"-c", "credential.helper=!f() { echo username=oauth2; echo \"password=$FORGE_TOKEN\"; }; f"},
		Env:  map[string]string{"FORGE_TOKEN": workerToken},
	}, nil
}
