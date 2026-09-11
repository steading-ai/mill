package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	VCSURL       string   `yaml:"vcs-url"`
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
	// The cache lineages (docs/sandbox.md §Caching): the size estimate past
	// which a global lineage is reset at promotion, and past which a
	// repo-scoped one is flagged. Sizes such as `20G`.
	CacheGlobalCap string `yaml:"cache-global-cap"`
	CacheRepoCap   string `yaml:"cache-repo-cap"`
	LogDir         string `yaml:"log-dir"`
	EnvMode        string `yaml:"env_mode"`
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

// defaultConfigPath is where the file lives when nothing names one.
func defaultConfigPath() string {
	if path := os.Getenv(configEnv); path != "" {
		return path
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "mill", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mill", "config.yaml")
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
