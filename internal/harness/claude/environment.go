package claude

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/workspace"
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

// envFloor is what a sidecar keeps of the mill's environment in strict mode.
// It is the workspace's floor, [workspace.FloorNames], stated there with the
// reason for each variable: the same list the local backend gives every
// child, so a session and a check step stand on one PATH.
var envFloor = workspace.FloorNames

// deniedNames and deniedPrefixes are what never reaches a sidecar or a
// session, in either mode, whatever a step asks for.
//
// The list is not configurable and is not meant to be. A credential in a
// session's environment is a credential its Bash tool can print, and the
// model is reached on the account's login or through the workspace's
// gateway, never on a key of the host's. ANTHROPIC_BASE_URL and
// ANTHROPIC_AUTH_TOKEN are deliberately not here: they are how a gateway is
// reached, and the workspace sets them after this filter. The Claude Code
// markers are here for a different reason: an interactive session's
// variables make the SDK believe it is nested inside one. So is the host's
// git configuration: what a session commits as is the workspace's answer,
// and an inherited variable that overrode it would be the defect this list
// exists to make impossible.
var (
	deniedNames = []string{
		// A key here is a key a tool can echo. The step's own bearer to the
		// gateway is the workspace's to set, and it is not either of these.
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN",
		// The mill's own database.
		"DATABASE_URL",
		// The marker of an interactive Claude Code session.
		"CLAUDECODE",
	}
	deniedPrefixes = []string{
		// The host's git configuration, in the one form that outranks a
		// file: GIT_CONFIG_COUNT and its key/value pairs. The workspace's
		// own GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are set after this
		// filter, because who a session commits as is the workspace's
		// answer and never the mill's ambient environment's.
		"GIT_CONFIG_",
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
// was arrived at. The socket, the token and the workspace's own environment
// — its git identity, its proxy, its model gateway — are added last, so
// nothing a step asked for can shadow the session's only grant, the identity
// it commits under, or the way it reaches out.
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

	// A sandboxed sidecar takes none of the mill's own environment as a
	// floor: the machine it enters is where HOME, PATH and the rest come
	// from (docs/sandbox.md §Exec, §"Session identity"). Only what the step
	// explicitly named is copied from the host, deny-listed names never, and
	// the session's grant, identity and egress are set over it below.
	if a.sandbox {
		return a.sandboxEnvironment(session, asked)
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
	// The workspace's own variables go on with them, in both modes and
	// after the deny filter: the run's commit identity and signing key
	// (GIT_CONFIG_GLOBAL and the GIT_AUTHOR_*/GIT_COMMITTER_* four), the
	// proxy every outbound connection takes (HTTPS_PROXY, HTTP_PROXY,
	// NO_PROXY), and the gateway the model is reached through with the step
	// token as its bearer (ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN). They
	// are the workspace's to decide, so a step's carve-out and the host's
	// own `~/.gitconfig` or proxy alike are past having a say by the time
	// the sidecar starts. The token among them is the step's: the map is
	// composed per step, after the workspace rotates, so each sidecar
	// carries the bearer the gateway is checking against and never the one
	// before it.
	for name, value := range session.Workspace.Env {
		env[name] = value
	}
	return env, harness.Environment{Passthrough: asked, Stripped: stripped}, nil
}

// sandboxEnvironment is the environment a sidecar entering a sandbox is
// spawned with: none of the mill's own, so the machine's HOME, PATH and
// XDG_RUNTIME_DIR stand. The step's `env.passthrough` still copies the
// named host variables — the one thing a step asked for by name — and the
// session's socket, token, identity and egress are set last, exactly as they
// are for a host sidecar, so nothing a step named can shadow them.
func (a *Adapter) sandboxEnvironment(session harness.Session, asked []string) (map[string]string, harness.Environment, error) {
	env := map[string]string{}
	for _, name := range asked {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	env[socketVar] = session.Workspace.Socket
	env[tokenVar] = session.Workspace.Token
	for name, value := range session.Workspace.Env {
		env[name] = value
	}
	return env, harness.Environment{Passthrough: asked}, nil
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

func onFloor(name string) bool { return workspace.OnFloor(name) }

func contains(names []string, name string) bool {
	for _, one := range names {
		if one == name {
			return true
		}
	}
	return false
}
