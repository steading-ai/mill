package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// The configuration file.
//
// A worker's settings are the long tail of a systemd ExecStart line: which
// projects, which labels, where dagger's socket is, where workspaces go.
// Putting them in a file keeps the unit one line and lets an operator change
// what the mill works on without editing a unit and reloading systemd.
//
// A flag always wins. The file is only consulted for a flag the command line
// did not set, so `mill work --concurrency 2` means two whatever the file
// says, and `mill work` alone means whatever the file says. A missing file
// is not an error: every setting in it has a default.

// configEnv names the file, for a unit that would rather set an environment
// variable than pass a flag.
const configEnv = "MILL_CONFIG"

// Config is the settings a file may carry. The keys are the long flag
// names, so the file and the command line say the same words — with one
// exception, `env_mode`, which is spelled the way the deployment that sets
// it spells it.
type Config struct {
	Socket       string   `yaml:"socket"`
	DaggerSocket string   `yaml:"dagger-socket"`
	Projects     []string `yaml:"project"`
	Labels       []string `yaml:"label"`
	Concurrency  int      `yaml:"concurrency"`
	Workflow     string   `yaml:"workflow"`
	Tier         string   `yaml:"tier"`
	WorkflowDir  string   `yaml:"workflow-dir"`
	Sidecar      string   `yaml:"sidecar"`
	Origin       string   `yaml:"origin"`
	Ref          string   `yaml:"ref"`
	Identity     string   `yaml:"identity"`
	Name         string   `yaml:"name"`
	WorkspaceDir string   `yaml:"workspace-dir"`
	Workspace    string   `yaml:"workspace"`
	SandboxRoot  string   `yaml:"sandbox-root"`
	// The machine backend's images (docs/sandbox.md §Images): where the
	// subvolumes are, which Harbor they come from, a pin that stands in
	// for `latest`, how long a derived build may take, and how long an
	// unbooted image lives. `base-image` is a tag or a digest.
	MachinesDir      string `yaml:"machines-dir"`
	ImageRegistry    string `yaml:"image-registry"`
	ImageRepository  string `yaml:"image-repository"`
	BaseImage        string `yaml:"base-image"`
	ImageBuildBudget string `yaml:"image-build-budget"`
	ImageExpiry      string `yaml:"image-expiry"`
	// The cache lineages (docs/sandbox.md §Caching, §Eviction): the size
	// estimate past which a global lineage is reset at promotion — for
	// all of them, and per lineage name — and past which a repo-scoped one
	// is flagged, sizes such as `20G`; how long a repo lineage goes
	// unrestored before eviction deletes it, a window such as `30d`; and
	// the free-space watermarks eviction starts below and stops above,
	// each a share such as `10%` or a size such as `50G`.
	CacheGlobalCap     string            `yaml:"cache-global-cap"`
	CacheGlobalCaps    map[string]string `yaml:"cache-global-caps"`
	CacheRepoCap       string            `yaml:"cache-repo-cap"`
	CacheExpiry        string            `yaml:"cache-expiry"`
	CacheHighWatermark string            `yaml:"cache-high-watermark"`
	CacheLowWatermark  string            `yaml:"cache-low-watermark"`
	// The machine backend's host facts (docs/sandbox.md §Provision,
	// §Network, §"Skill mount"): the worker's skill directory bound
	// read-only into every machine, the run-state directory the socket
	// directories are made under, and the proxy every machine leaves
	// through and what bypasses it. Each has the default homestead fixes
	// on `core`.
	SkillDir string `yaml:"skill-dir"`
	// SidecarDir is where the worker materialises this binary's sidecar,
	// bound read-only into every machine beside the skill (docs/sandbox.md
	// §"Skill mount"). It defaults, like the skill, to the worker's share
	// root, the one the helper's policy admits.
	SidecarDir string `yaml:"sidecar-dir"`
	// WorkflowCopyDir is where the worker materialises the workflow
	// directory it loaded from, bound read-only into every machine so a
	// check step's scripts are there (docs/sandbox.md §"Skill mount"). It
	// defaults, like the skill, to the worker's share root.
	WorkflowCopyDir string `yaml:"workflow-copy-dir"`
	RunStateDir     string `yaml:"run-state-dir"`
	Proxy           string `yaml:"proxy"`
	NoProxy         string `yaml:"no-proxy"`
	// Nesting (docs/sandbox.md §Self-hosting): the repositories whose
	// `[machine].nested` request is granted, each by its path on the forge
	// (`wellspring/mill`) or its origin URL, and the dagger checkout a
	// nested machine gets bound read-only for its test fixture. Spelled
	// with underscores the way env_mode is: a deployment writes them.
	NestedRepos    []string `yaml:"nested_repos"`
	DaggerCheckout string   `yaml:"dagger_checkout"`
	// The reconciliation sweep (docs/sandbox.md §"External state"): the
	// forge group the acceptance suite makes its scratch projects in, by
	// path or numeric id, and how old a scratch project must be before the
	// worker sweeps it. An empty namespace sweeps no forge; the budget
	// defaults to the suite's own timeout.
	ScratchNamespace string `yaml:"scratch-namespace"`
	ScratchBudget    string `yaml:"scratch-budget"`
	LogDir           string `yaml:"log-dir"`
	EnvMode          string `yaml:"env_mode"`
	// NotifyCommand is spelled the way env_mode is, for the same reason: a
	// deployment sets it, and a deployment spells it with an underscore.
	NotifyCommand string `yaml:"notify_command"`
	Work          bool   `yaml:"work"`
	// The credential gateway (docs/sandbox.md §Credentials). Gateway turns
	// it on; the rest name where it listens, what it forwards to, and the
	// file holding the credential it presents upstream.
	Gateway             bool   `yaml:"gateway"`
	GatewayListen       string `yaml:"gateway-listen"`
	GatewayUpstream     string `yaml:"gateway-upstream"`
	ModelCredentialFile string `yaml:"model-credential-file"`
	// ModelCredentialMinted is the date, YYYY-MM-DD, the subscription
	// credential was minted, which `mill doctor` warns from at eleven
	// months. Spelled the way env_mode is: a deployment writes it.
	ModelCredentialMinted string `yaml:"model_credential_minted"`
}

// systemConfigPath is where the package installs the file (nfpm.yaml), for
// a host that runs mill from a system unit rather than an account.
const systemConfigPath = "/etc/mill/config.yaml"

// defaultConfigPath is where the file lives when nothing names one:
// $MILL_CONFIG; else the account's own, under $XDG_CONFIG_HOME or
// ~/.config; else the system's, when the account has none and the package
// installed one. The account's wins so an operator's file is never
// shadowed by the package's; the system's is found so `mill doctor` on a
// packaged host reads the settings the service runs with.
func defaultConfigPath() string {
	if path := os.Getenv(configEnv); path != "" {
		return path
	}
	return configPath(userConfigPath(), systemConfigPath)
}

// userConfigPath is the account's own file, whether or not it exists.
func userConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "mill", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mill", "config.yaml")
}

// configPath picks between the account's file and the system's: the
// account's when it exists or the system's does not, so a host with neither
// still reports the account's path as the place a file would be read from.
func configPath(user, system string) string {
	if user != "" {
		if _, err := os.Stat(user); err == nil {
			return user
		}
	}
	if _, err := os.Stat(system); err == nil {
		return system
	}
	return user
}

// LoadConfig reads the file at path. A path that does not exist yields the
// zero config and no error; a path that exists and does not parse is an
// error, because a worker started with settings nobody can read is worse
// than one that refuses to start.
func LoadConfig(path string) (Config, error) {
	var config Config
	if path == "" {
		return config, nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("mill: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(body, &config); err != nil {
		return config, fmt.Errorf("mill: %s: %w", path, err)
	}
	return config, nil
}

// deploymentConfig is the deployment's own configuration file, for a caller
// with no flag of its own to read a setting from. A file that does not parse
// yields the zero config rather than an error: the caller is `mill mcp`'s
// doctor tool, and a probe that cannot be configured reports that it is not
// configured.
func deploymentConfig() Config {
	config, err := LoadConfig(defaultConfigPath())
	if err != nil {
		return Config{}
	}
	return config
}

// unset reports whether the command line left a flag alone, which is the
// only case the file speaks to.
func unset(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	return flag != nil && !flag.Changed
}

// checkNamedConfig refuses a configuration file the operator named and that
// is not there.
//
// The default location is a place to look rather than an instruction:
// nothing named it, every setting in it has a default, and a mill with no
// file at all is a working mill. A path given as --config or as $MILL_CONFIG
// is the opposite. Reading it as an empty file starts a worker on defaults
// nobody asked for — the projects, the socket and the credential all falling
// back to whatever the environment happens to hold, which for a worker means
// claiming from a project the operator never named. A typo is a mistake, not
// a default.
func checkNamedConfig(cmd *cobra.Command, path string) error {
	if path == "" || (unset(cmd, "config") && os.Getenv(configEnv) == "") {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("mill: configuration file %s does not exist; "+
				"name one that does with --config or $%s, or name none and run on the defaults", path, configEnv)
		}
		return fmt.Errorf("mill: %s: %w", path, err)
	}
	return nil
}

// configKey is one key of the file as the worker reads it: the key as the
// file spells it, and the flag it stands in for. A key with no flag is the
// machine backend's, which the file alone sets whenever it is read.
type configKey struct {
	key  string
	flag string
}

// configKeys is every key applyConfig reads. A command honours one when it
// reads the file at all — it declares --config — and declares the key's
// flag, or the key has none; configKeyTable renders that per command and
// docs/tools.md §Configuration carries the rendering. The order is the
// Config struct's.
var configKeys = []configKey{
	{"socket", "socket"},
	{"dagger-socket", "dagger-socket"},
	{"project", "project"},
	{"label", "label"},
	{"concurrency", "concurrency"},
	{"workflow", "workflow"},
	{"tier", "tier"},
	{"workflow-dir", "workflow-dir"},
	{"sidecar", "sidecar"},
	{"origin", "origin"},
	{"ref", "ref"},
	{"identity", "identity"},
	{"name", "name"},
	{"workspace-dir", "workspace-dir"},
	{"workspace", "workspace"},
	{"sandbox-root", "sandbox-root"},
	{"machines-dir", ""},
	{"image-registry", ""},
	{"image-repository", ""},
	{"base-image", ""},
	{"image-build-budget", ""},
	{"image-expiry", ""},
	{"cache-global-cap", ""},
	{"cache-global-caps", ""},
	{"cache-repo-cap", ""},
	{"cache-expiry", ""},
	{"cache-high-watermark", ""},
	{"cache-low-watermark", ""},
	{"skill-dir", ""},
	{"sidecar-dir", ""},
	{"workflow-copy-dir", ""},
	{"run-state-dir", ""},
	{"proxy", ""},
	{"no-proxy", ""},
	{"nested_repos", ""},
	{"dagger_checkout", ""},
	{"scratch-namespace", ""},
	{"scratch-budget", ""},
	{"log-dir", "log-dir"},
	{"env_mode", "env-mode"},
	{"notify_command", "notify-command"},
	{"gateway", "gateway"},
	{"gateway-listen", "gateway-listen"},
	{"gateway-upstream", "gateway-upstream"},
	{"model-credential-file", "model-credential-file"},
}

// unreadKeys are the file's keys no worker reads: `work` is parsed and
// used by nothing, and `model_credential_minted` is `mill doctor`'s. They
// are listed so the table shows them as what they are rather than leaving
// them out.
var unreadKeys = []string{"work", "model_credential_minted"}

// honoursKey reports whether a command reads key from the file: it reads
// the file at all, and the key's flag — the one the file's value fills when
// the command line leaves it alone — is among the command's, or the key has
// none.
func honoursKey(cmd *cobra.Command, key configKey) bool {
	if cmd.Flags().Lookup("config") == nil {
		return false
	}
	return key.flag == "" || cmd.Flags().Lookup(key.flag) != nil
}

// configKeyTable renders which of the file's keys each of `run`, `work`
// and `serve` honours, as the Markdown table docs/tools.md carries. The
// columns are the commands as an operator invokes them; `serve` reads the
// file only with --work, which is what its column says.
func configKeyTable(root *cobra.Command) string {
	columns := []struct{ heading, name string }{
		{"`run`", "run"}, {"`work`", "work"}, {"`serve --work`", "serve"},
	}
	var b strings.Builder
	b.WriteString("| Key |")
	for _, column := range columns {
		b.WriteString(" " + column.heading + " |")
	}
	b.WriteString("\n|---|")
	for range columns {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	row := func(key string, honours func(*cobra.Command) bool) {
		b.WriteString("| `" + key + "` |")
		for _, column := range columns {
			cmd, _, err := root.Find([]string{column.name})
			if err == nil && honours(cmd) {
				b.WriteString(" yes |")
			} else {
				b.WriteString(" |")
			}
		}
		b.WriteString("\n")
	}
	for _, key := range configKeys {
		row(key.key, func(cmd *cobra.Command) bool { return honoursKey(cmd, key) })
	}
	for _, key := range unreadKeys {
		row(key, func(*cobra.Command) bool { return false })
	}
	return b.String()
}
