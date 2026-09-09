package claude

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ivy/mill/internal/harness"
)

// The environment a sidecar is spawned with.
//
// The mill's own process is not a floor. It carries a database URL, a forge
// token, the dagger project it was started for, and — when an operator runs
// `mill run` from a Claude Code session — the markers that make the SDK
// think it is nested. None of that is a session's business, and the first
// end-to-end run handed the sidecar all of it.
//
// What replaces it is a mode rather than one fixed floor, because the two
// audiences differ. A person trying mill on a laptop wants their sessions to
// see what their shell sees — GOFLAGS, GOPRIVATE, a proxy, SSH_AUTH_SOCK —
// and should not have to learn a carve-out before the first run works. A
// deployment wants the opposite and says so. So `inherit` is the default and
// `strict` is what a unit file sets. In both modes the deny list applies:
// what it names never reaches a sidecar, whatever the mode and whatever the
// step asks for. See docs/architecture.md §"Skill steps: the harness
// adapter".

// EnvMode says where a sidecar's environment comes from.
type EnvMode string

const (
	// EnvInherit hands the sidecar the mill's own environment minus the
	// deny list. It is the default: `mill run` from a shell behaves like
	// the shell.
	EnvInherit EnvMode = "inherit"
	// EnvStrict hands the sidecar the floor, plus what the step's options
	// explicitly named, minus the deny list. A deployment sets it.
	EnvStrict EnvMode = "strict"
)

// DefaultEnvMode is the mode an adapter that was told nothing runs under.
const DefaultEnvMode = EnvInherit

// ParseEnvMode reads a mode's name, as a flag or a configuration file
// spells it.
func ParseEnvMode(name string) (EnvMode, error) {
	switch EnvMode(name) {
	case EnvInherit:
		return EnvInherit, nil
	case EnvStrict:
		return EnvStrict, nil
	default:
		return "", fmt.Errorf("claude: %q is not an environment mode: inherit or strict", name)
	}
}

// envFloor is what a sidecar keeps of the mill's environment in strict mode:
// enough for a process to start, find the account's own Claude Code login,
// and run git and the project's toolchain, and nothing that authenticates
// the mill to anything else.
//
// It is the same list as ENVIRONMENT_PASSTHROUGH in sidecar/src/session.ts,
// which is the same question one hop further along — what the session's
// tools inherit from the sidecar. The two are stated separately because they
// are two boundaries; change one and look at the other.
//
//	HOME             the SDK finds ~/.claude/.credentials.json and writes
//	                 transcripts under ~/.claude/projects; git reads ~/.gitconfig
//	PATH             the sidecar finds node, the SDK its claude binary, the
//	                 session git and the project's toolchain
//	LANG, LC_ALL     encoding; without them Node and git fall back to ASCII
//	                 and mangle non-ASCII in bodies and diffs
//	LOGNAME, USER    git's identity fallback, so a commit works without
//	                 user.name set
//	SHELL            the SDK's Bash tool runs the account's shell
//	TERM             tools that check for a terminal behave predictably
//	TMPDIR           the SDK and git write temporary files; a deployment
//	                 points it inside the runtime directory
//	TZ               timestamps in commits and logs match the host's
//	XDG_RUNTIME_DIR  the sidecar's own sockets and the SDK's IPC live there
var envFloor = []string{
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

// deniedNames and deniedPrefixes are what never reaches a sidecar or a
// session, in either mode, whatever a step asks for.
//
// The list is not configurable and is not meant to be. A credential in a
// session's environment is a credential its Bash tool can print, and the
// login the SDK needs comes from HOME rather than from a key. The Claude
// Code markers are here for a different reason: an interactive session's
// variables make the SDK believe it is nested inside one.
var (
	deniedNames = []string{
		// The login comes from HOME. A key here is a key a tool can echo.
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN",
		// The mill's own database.
		"DATABASE_URL",
		// The marker of an interactive Claude Code session.
		"CLAUDECODE",
	}
	deniedPrefixes = []string{
		// The mill's own settings and credentials, MILL_FORGE_TOKEN among
		// them.
		"MILL_",
		// Dagger's socket and project: a session speaks to the work graph
		// through nothing.
		"DG_",
		// Everything an interactive Claude Code session exports about
		// itself, CLAUDE_CODE_OAUTH_TOKEN included.
		"CLAUDE_CODE_",
	}
)

// denied reports whether a variable is one the mill never hands on.
func denied(name string) bool {
	for _, one := range deniedNames {
		if name == one {
			return true
		}
	}
	for _, prefix := range deniedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// environment is what the sidecar is spawned with, and the record of how it
// was arrived at. The socket and the token are added last, so nothing a step
// asked for can shadow the session's only grant.
//
// A step asking for a deny-listed variable is refused here — before the
// sidecar is spawned and in either mode — naming the variable, because the
// workflow line that asked for it is what has to change.
func (a *Adapter) environment(session harness.Session) (map[string]string, harness.Environment, error) {
	asked := passthroughOf(session.Options)
	for _, name := range asked {
		if denied(name) {
			return nil, harness.Environment{}, fmt.Errorf(
				"claude: the step asks for %s to be passed to its session, and %s is never given to one", name, name)
		}
	}

	env := map[string]string{}
	var stripped []string
	seen := map[string]bool{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if denied(name) {
			if !seen[name] {
				seen[name] = true
				stripped = append(stripped, name)
			}
			continue
		}
		if a.envMode == EnvStrict && !onFloor(name) && !contains(asked, name) {
			continue
		}
		env[name] = value
	}
	sort.Strings(stripped)

	env[socketVar] = session.Workspace.Socket
	env[tokenVar] = session.Workspace.Token
	return env, harness.Environment{Passthrough: asked, Stripped: stripped}, nil
}

// passthroughOf is the variables a step's options asked to have carried from
// the mill's environment: `env.passthrough` in its `harness.claude` block.
//
// This is the one key of the options document the adapter reads, and it
// reads it because the decision is this side's: what a child process is
// given is not something the child can be trusted to decide. Anything whose
// shape is not a list of names is left alone rather than guessed at — the
// sidecar owns the schema and refuses the document with the key named
// (sidecar/src/options.ts), which is a better error than any this side could
// write.
func passthroughOf(options map[string]any) []string {
	block, ok := options["env"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := block["passthrough"].([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		if name, ok := entry.(string); ok && name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

func onFloor(name string) bool { return contains(envFloor, name) }

func contains(names []string, name string) bool {
	for _, one := range names {
		if one == name {
			return true
		}
	}
	return false
}
