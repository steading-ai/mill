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
// names, so the file and the command line say the same words.
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
	LogDir       string   `yaml:"log-dir"`
	Work         bool     `yaml:"work"`
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

// unset reports whether the command line left a flag alone, which is the
// only case the file speaks to.
func unset(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	return flag != nil && !flag.Changed
}
