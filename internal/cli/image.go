package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// `mill image` is the operator's side of docs/sandbox.md §Images: build and
// publish the base the machines boot from, see which image subvolumes this
// host holds, and let go of the ones nothing has booted in a while.
//
// The worker builds derived images itself, on first use; none of these
// verbs is on a run's path. `build-base` is the timer's command
// (contrib/systemd/mill-image-build.timer), and the one that runs a
// container on the host outside any machine.

// imageOptions are the flags the image verbs share, and the configuration
// keys behind them.
type imageOptions struct {
	config      string
	sandboxRoot string
	machinesDir string
	registry    string
	repository  string
	sudo        string
	// build-base
	context        string
	push           bool
	robotFile      string
	buildBudget    time.Duration
	baseImage      string
	imageExpiryRaw string
	// prune
	dryRun bool
}

// defaultImageSudo is how the image verbs reach the image directory, which
// is root's on the host (docs/sandbox.md §Storage): through sudo, never
// prompting. `none` runs them as the caller, for a host where the
// directory is the caller's.
const defaultImageSudo = "sudo"

func newImageCommand() *cobra.Command {
	opts := imageOptions{}
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build the base image, and list and prune the image subvolumes on this host",
		Long: `A machine boots from an ephemeral snapshot of an image subvolume: the base
the operator publishes to Harbor, or a repository's derived image — base
plus the packages its mill.toml declares — which the worker builds on first
use (docs/sandbox.md §Images).

"build-base" builds the base from images/base and tags it <date> and latest;
with --push it publishes both to Harbor.
"ls" lists the image subvolumes this host holds, with what each was built
from and when a machine last booted it.
"prune" removes the ones nothing has booted within the expiry.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	f := cmd.PersistentFlags()
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml)")
	f.StringVar(&opts.sandboxRoot, "sandbox-root", machine.DefaultRoot, "the sandbox root; image locks and records live under <root>/images")
	f.StringVar(&opts.machinesDir, "machines-dir", machine.DefaultMachines, "directory the image subvolumes live in")
	f.StringVar(&opts.registry, "image-registry", image.DefaultRegistry, "the Harbor host the base is published to and pulled from")
	f.StringVar(&opts.repository, "image-repository", image.DefaultRepository, "the base image's repository on that host")
	f.StringVar(&opts.sudo, "sudo", defaultImageSudo, "how the image directory, which is root's, is written: the sudo command, or none")
	cmd.AddCommand(newImageBuildBaseCommand(&opts), newImageLsCommand(&opts), newImagePruneCommand(&opts))
	return cmd
}

func newImageBuildBaseCommand(opts *imageOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build-base",
		Short: "Build the base image from images/base, and push it with --push",
		Long: `Build the base image on this host — podman build, outside any machine —
from the Containerfile under --context, with the Harbor host as its
HARBOR_HOST build argument, and tag it <registry>/<repository>:<today> and
:latest.

"--push" logs in with the robot credential — one line, robot:secret, from
$CREDENTIALS_DIRECTORY/harbor-robot or --robot-credential-file — pushes
both tags, and logs out. The worker resolves :latest to a digest at each
provision, so a pushed base takes effect on the next run; a pin in worker
configuration (base-image) is the rollback.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImageBuildBase(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.context, "context", "images/base", "directory holding the base Containerfile")
	cmd.Flags().BoolVar(&opts.push, "push", false, "push the dated and latest tags to the registry")
	cmd.Flags().StringVar(&opts.robotFile, "robot-credential-file", image.DefaultRobotPath(), "file holding the Harbor robot credential as robot:secret (default: $"+image.CredentialsDirectoryEnv+"/"+image.RobotCredentialName+")")
	return cmd
}

func runImageBuildBase(cmd *cobra.Command, opts *imageOptions) error {
	store, err := imageStore(cmd, opts)
	if err != nil {
		return err
	}
	build := image.BaseBuild{Context: opts.context, Push: opts.push}
	if opts.push {
		robot, err := image.LoadRobot(opts.robotFile)
		if err != nil {
			return fmt.Errorf("image build-base: %w", err)
		}
		build.Robot = robot
	}
	result, err := store.BuildBase(cmd.Context(), build)
	if err != nil {
		return fmt.Errorf("image build-base: %w", err)
	}
	out := cmd.OutOrStdout()
	for _, tag := range result.Tags {
		verb := "built"
		if result.Pushed {
			verb = "pushed"
		}
		_, _ = fmt.Fprintf(out, "%s %s\n", verb, tag)
	}
	_, _ = fmt.Fprintf(out, "digest %s\n", result.Digest.OCI())
	return nil
}

func newImageLsCommand(opts *imageOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the image subvolumes this host holds",
		Long: `List every image subvolume the worker built and that is still on this
host: its digest, the base it derives from, the packages over the base,
when it was built and when a machine last booted it. Least recently used
first, which is the order prune takes them in.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := imageStore(cmd, opts)
			if err != nil {
				return err
			}
			return runImageLs(cmd, store)
		},
	}
}

func runImageLs(cmd *cobra.Command, store *image.Store) error {
	manifests, err := store.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("image ls: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(manifests) == 0 {
		_, _ = fmt.Fprintf(out, "no image subvolumes under %s\n", store.Config().Machines)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "DIGEST\tBASE\tPACKAGES\tBUILT\tLAST BOOT")
	for _, m := range manifests {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			m.Digest.Short(), baseText(m), packagesText(m.Packages),
			m.BuiltAt.UTC().Format(time.RFC3339), bootText(m))
	}
	return w.Flush()
}

// baseText is the base column: `base` for the base's own subvolume.
func baseText(m image.Manifest) string {
	if m.Digest == m.Base {
		return "base"
	}
	return m.Base.Short()
}

func packagesText(packages []string) string {
	if len(packages) == 0 {
		return "-"
	}
	return strings.Join(packages, ",")
}

func bootText(m image.Manifest) string {
	if m.LastBootedAt.IsZero() {
		return "never"
	}
	return m.LastBootedAt.UTC().Format(time.RFC3339)
}

func newImagePruneCommand(opts *imageOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove the image subvolumes nothing has booted within the expiry",
		Long: `Delete every image subvolume whose last boot — or its build, when nothing
has booted it — is older than the expiry: 30 days unless --expiry or the
configuration's image-expiry says otherwise (docs/sandbox.md §Eviction).

Expiry only. Free-space watermarks are the eviction node's. A running
machine's ephemeral root is a snapshot of its own and is untouched by the
source subvolume going.

"--dry-run" lists what would go and removes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := imageStore(cmd, opts)
			if err != nil {
				return err
			}
			expiry, err := parseWindow(opts.imageExpiryRaw)
			if err != nil {
				return fmt.Errorf("--expiry: %q is not a window such as 720h or 30d", opts.imageExpiryRaw)
			}
			return runImagePrune(cmd, store, expiry, opts.dryRun)
		},
	}
	cmd.Flags().StringVar(&opts.imageExpiryRaw, "expiry", "30d", "remove images not booted for longer than this, such as 30d")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "list what would be removed and remove nothing")
	return cmd
}

func runImagePrune(cmd *cobra.Command, store *image.Store, expiry time.Duration, dryRun bool) error {
	ctx := cmd.Context()
	var pruned []image.Manifest
	var err error
	if dryRun {
		pruned, err = expired(ctx, store, expiry)
	} else {
		pruned, err = store.Prune(ctx, expiry)
	}
	if err != nil {
		return fmt.Errorf("image prune: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(pruned) == 0 {
		_, _ = fmt.Fprintf(out, "nothing to remove under %s\n", store.Config().Machines)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "DIGEST\tPACKAGES\tLAST BOOT\tACTION")
	for _, m := range pruned {
		action := "removed"
		if dryRun {
			action = "would remove"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Digest.Short(), packagesText(m.Packages), bootText(m), action)
	}
	return w.Flush()
}

// expired is what prune would take, for a dry run.
func expired(ctx context.Context, store *image.Store, expiry time.Duration) ([]image.Manifest, error) {
	manifests, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := store.Config().Now().Add(-expiry)
	var out []image.Manifest
	for _, m := range manifests {
		if m.LastUsed().Before(cutoff) {
			out = append(out, m)
		}
	}
	return out, nil
}

// imageStore is the store the flags describe, over the systemd driver:
// every host command through the chokepoint, the image directory through
// sudo unless told otherwise. The configuration file fills in what the
// command line left alone.
func imageStore(cmd *cobra.Command, opts *imageOptions) (*image.Store, error) {
	if err := applyImageConfig(cmd, opts); err != nil {
		return nil, err
	}
	sudo := opts.sudo
	if sudo == "none" {
		sudo = ""
	}
	host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: sudo})
	return image.New(host, imageConfig(opts)), nil
}

// imageConfig is the image configuration as the flags spell it.
func imageConfig(opts *imageOptions) image.Config {
	return image.Config{
		Root:        opts.sandboxRoot,
		Machines:    opts.machinesDir,
		Registry:    opts.registry,
		Repository:  opts.repository,
		Pin:         opts.baseImage,
		BuildBudget: opts.buildBudget,
	}
}

// applyImageConfig fills in every image flag the command line left alone
// from the configuration file.
func applyImageConfig(cmd *cobra.Command, opts *imageOptions) error {
	if err := checkNamedConfig(cmd, opts.config); err != nil {
		return err
	}
	config, err := LoadConfig(opts.config)
	if err != nil {
		return err
	}
	targets := map[string]*string{
		"sandbox-root": &opts.sandboxRoot, "machines-dir": &opts.machinesDir,
		"image-registry": &opts.registry, "image-repository": &opts.repository,
		"expiry": &opts.imageExpiryRaw,
	}
	values := map[string]string{
		"sandbox-root": config.SandboxRoot, "machines-dir": config.MachinesDir,
		"image-registry": config.ImageRegistry, "image-repository": config.ImageRepository,
		"expiry": config.ImageExpiry,
	}
	for name, target := range targets {
		if value := values[name]; value != "" && unset(cmd, name) {
			*target = value
		}
	}
	opts.baseImage = config.BaseImage
	if config.ImageBuildBudget != "" {
		budget, err := time.ParseDuration(config.ImageBuildBudget)
		if err != nil {
			return fmt.Errorf("mill: %s: image-build-budget: %w", opts.config, err)
		}
		opts.buildBudget = budget
	}
	return nil
}
