package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
)

// Command is one process to spawn: what to run, where, and what the step's
// capabilities let it see of the environment.
type Command struct {
	// Path is the program to run, resolved against PATH when it is not
	// absolute.
	Path string
	// Args are the arguments after argv[0].
	Args []string
	// Dir is the working directory. Empty means the caller's.
	Dir string
	// Env is what the caller adds to the child's environment. It is added
	// over the workspace's own floor rather than replacing it, so a caller
	// names only the variables that belong to its step and nothing else.
	Env map[string]string
	// ClearEnv makes Env the whole of the child's environment: the floor is
	// not consulted at all. A caller that has decided every variable its
	// child may see — the harness adapter under an environment mode
	// (docs/architecture.md §"Skill steps: the harness adapter") — says so
	// here rather than by naming the floor's variables again.
	ClearEnv bool
}

// FloorNames are the variables of the mill's own environment that a child
// under the local backend inherits, and that a sidecar keeps in strict mode:
// enough for a process to start and run git and the project's toolchain,
// and nothing that authenticates the mill to anything else. A check step's
// `make test` or `mise exec` resolves on this PATH; without it a shell falls
// back to its built-in default and the toolchain a deployment put on the
// unit's PATH is not there.
//
// It is the same list as ENVIRONMENT_PASSTHROUGH in sidecar/src/session.ts,
// which is the same question one hop further along — what the session's
// tools inherit from the sidecar. The two are stated separately because they
// are two boundaries; change one and look at the other. What the workspace
// decided is not part of either: the git identity, the proxy and the model
// gateway are set past the floor, from the workspace's environment.
//
//	HOME             the shell, the SDK's transcripts under
//	                 ~/.claude/projects, and the harness's own state. Not a
//	                 login: in a machine there is none, and the model is
//	                 reached through the gateway the workspace names. Not
//	                 git's identity: the workspace's GIT_CONFIG_GLOBAL
//	                 displaces the account's configuration on every session
//	PATH             the sidecar finds node, the SDK its claude binary, the
//	                 session git, and a check and a session the project's
//	                 toolchain
//	LANG, LC_ALL     encoding; without them Node and git fall back to ASCII
//	                 and mangle non-ASCII in bodies and diffs
//	LOGNAME, USER    the account a tool reports itself running as
//	SHELL            the SDK's Bash tool runs the account's shell
//	TERM             tools that check for a terminal behave predictably
//	TMPDIR           the SDK and git write temporary files; a deployment
//	                 points it inside the runtime directory
//	TZ               timestamps in commits and logs match the host's
//	XDG_RUNTIME_DIR  the sidecar's own sockets and the SDK's IPC live there
var FloorNames = []string{
	"HOME",
	"LANG",
	"LC_ALL",
	"LOGNAME",
	"PATH",
	"SHELL",
	"TERM",
	"TMPDIR",
	"TZ",
	"USER",
	"XDG_RUNTIME_DIR",
}

// Floor is the entries of environ whose names are on [FloorNames], in
// environ's order. It is never nil, so a [Spawner] given it inherits
// nothing more.
func Floor(environ []string) []string {
	floor := []string{}
	for _, entry := range environ {
		if OnFloor(nameOf(entry)) {
			floor = append(floor, entry)
		}
	}
	return floor
}

// OnFloor reports whether name is one of [FloorNames].
func OnFloor(name string) bool {
	for _, one := range FloorNames {
		if one == name {
			return true
		}
	}
	return false
}

// DeadSockets are the DG_SOCKET and MILL_SOCKET a workspace hands every
// process it spawns: real, absolute, non-empty paths under dir, so
// envOr/defaultSocketPath's "empty means unset" fallback to the compiled-in
// production sockets never triggers, and scoped to the run's own directory,
// so two runs never collide and Destroy's removal of dir cleans them up with
// everything else. Nothing ever binds either path — a dial against it fails
// rather than reaching a worker — which is what makes handing it out safe:
// scoped to dir, not necessarily visible from it, since the machine backend's
// dir is a host-side path its namespace never mounts and the value still
// closes the hole from the environment's side.
func DeadSockets(dir string) map[string]string {
	return map[string]string{
		"DG_SOCKET":   filepath.Join(dir, "no-dagger.sock"),
		"MILL_SOCKET": filepath.Join(dir, "no-mill.sock"),
	}
}

// MergeDeadSockets adds handle's [DeadSockets] into env, scoped to the
// directory holding handle.Socket. A workspace not yet attached has no
// handle and so no run directory to scope them under; env is left alone.
func MergeDeadSockets(env map[string]string, handle *Handle) {
	if handle == nil {
		return
	}
	for name, value := range DeadSockets(filepath.Dir(handle.Socket)) {
		env[name] = value
	}
}

// Result is how a spawned process ended: the raw facts, with none of the
// adapter's "never ran" convention layered on. Status is -1 when a signal
// ended the process, which is what Signal then says.
type Result struct {
	// Status is the exit status, or -1 when a signal ended the process.
	Status int32
	// Signal is the signal that killed the process, or 0 if none did.
	Signal int32
}

// Process is a spawned process and the pipes its caller talks to it over. It
// runs in a process group of its own, so a signal reaches everything it
// spawned and not just the child itself.
type Process struct {
	cmd *exec.Cmd

	// Stdin is the child's standard input. The caller closes it.
	Stdin io.WriteCloser
	// Stdout is the child's standard output, closed by Wait.
	Stdout io.ReadCloser
	// Stderr is the child's standard error, closed by Wait.
	Stderr io.ReadCloser
}

// Execer spawns a process. Everything mill runs on behalf of a step goes
// through an implementation of this, which is why os/exec is forbidden
// outside this package: sandboxing, credentials and cancellation then have
// exactly one place to live.
type Execer interface {
	Exec(ctx context.Context, command Command) (*Process, error)
}

// Spawner runs processes as the mill's own account, which is what the local
// workspace does. The machine workspace reaches its machine through the same
// spawner — its driver runs the host's systemd and btrfs commands through
// nothing else — and the remote workspace implements [Execer] with a
// boundary of its own; see docs/architecture.md §"The workspace".
type Spawner struct {
	// Floor is the environment every child inherits before the command's own
	// variables are added. Nil means the mill's own environment.
	Floor []string
}

// Exec spawns the command in a new process group.
func (l Spawner) Exec(ctx context.Context, command Command) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if command.Path == "" {
		return nil, errors.New("workspace: no command to run")
	}

	cmd := exec.Command(command.Path, command.Args...) //nolint:gosec // the command is the mill's own configuration, never a session's input
	cmd.Dir = command.Dir
	cmd.Env = environ(l.Floor, command)
	// A process group of its own is what makes cancellation reach the whole
	// tree a session spawns, not just the child mill can see.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	process := &Process{cmd: cmd}
	var err error
	if process.Stdin, err = cmd.StdinPipe(); err != nil {
		return nil, fmt.Errorf("workspace: stdin pipe: %w", err)
	}
	if process.Stdout, err = cmd.StdoutPipe(); err != nil {
		return nil, fmt.Errorf("workspace: stdout pipe: %w", err)
	}
	if process.Stderr, err = cmd.StderrPipe(); err != nil {
		return nil, fmt.Errorf("workspace: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("workspace: start %s: %w", command.Path, err)
	}
	return process, nil
}

// environ renders the floor with the command's own variables added over it,
// in a stable order so two identical commands produce identical children. A
// command that carries its whole environment starts from nothing instead.
func environ(floor []string, command Command) []string {
	own := command.Env
	switch {
	case command.ClearEnv:
		floor = nil
	case floor == nil:
		floor = os.Environ()
	}
	names := make([]string, 0, len(own))
	for name := range own {
		names = append(names, name)
	}
	sort.Strings(names)

	env := make([]string, 0, len(floor)+len(names))
	for _, entry := range floor {
		if _, overridden := own[nameOf(entry)]; !overridden {
			env = append(env, entry)
		}
	}
	for _, name := range names {
		env = append(env, name+"="+own[name])
	}
	return env
}

func nameOf(entry string) string {
	for i := range len(entry) {
		if entry[i] == '=' {
			return entry[:i]
		}
	}
	return entry
}

// Pid is the child's process id, which is also its process group id.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Signal sends a signal to the whole process group, so nothing the child
// spawned outlives it.
func (p *Process) Signal(sig syscall.Signal) error {
	pid := p.Pid()
	if pid <= 0 {
		return errors.New("workspace: process has not started")
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		// The group is already gone, which is the outcome the caller wanted.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("workspace: signal %v to group %d: %w", sig, pid, err)
	}
	return nil
}

// Wait reaps the process and reports how it ended. The caller must have read
// Stdout and Stderr to their end first: Wait closes them.
func (p *Process) Wait() Result {
	_ = p.cmd.Wait()
	state := p.cmd.ProcessState
	if state == nil {
		return Result{Status: -1}
	}
	result := Result{Status: int32(state.ExitCode())} //nolint:gosec // an exit status is a byte, or -1
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.Signal = int32(status.Signal())
	}
	return result
}
