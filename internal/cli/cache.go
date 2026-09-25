package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/machine/cache"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
	"github.com/steading-ai/mill/internal/workspace/machine/image"
)

// `mill cache` is the operator's side of docs/sandbox.md §Caching and
// §Eviction: which lineages this worker holds, how many generations each
// has, what the latest is estimated at, when a run last restored it, and
// whether it is over its cap; and `prune`, the eviction the worker runs at
// start and after each promotion, for a timer. The listing is read from
// the directory tree and each lineage's manifest.json, never from btrfs,
// which needs root to list subvolumes.

// cacheOptions are the flags the cache verbs share, and the configuration
// keys behind them.
type cacheOptions struct {
	config      string
	sandboxRoot string
	globalCap   string
	repoCap     string
	// prune: the policy, and the image side of it.
	expiry        string
	imageExpiry   string
	highWatermark string
	lowWatermark  string
	machinesDir   string
	sudo          string
}

func newCacheCommand() *cobra.Command {
	opts := cacheOptions{}
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "List the cache lineages this worker holds",
		Long: `A cache is a lineage: a named directory carried between runs as read-only
btrfs snapshots, one generation per promotion, under
<sandbox-root>/cache/<scope>/<name> (docs/sandbox.md §Caching).

"ls" lists every lineage with its scope, its generations, the latest
generation's size estimate, when a run last restored it, and whether it
is over its scope's cap.
"prune" runs eviction: expiry, then least recently used under the
free-space watermarks, lineages and images both (docs/sandbox.md
§Eviction).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	f := cmd.PersistentFlags()
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml, else /etc/mill/config.yaml)")
	f.StringVar(&opts.sandboxRoot, "sandbox-root", machine.DefaultRoot, "the sandbox root; lineages live under <root>/cache")
	f.StringVar(&opts.globalCap, "global-cap", cache.FormatSize(cache.DefaultGlobalCap), "size estimate past which a global lineage is reset at promotion, such as 20G")
	f.StringVar(&opts.repoCap, "repo-cap", cache.FormatSize(cache.DefaultRepoCap), "size estimate past which a repo-scoped lineage is flagged, such as 10G")
	cmd.AddCommand(newCacheLsCommand(&opts), newCachePruneCommand(&opts))
	return cmd
}

func newCachePruneCommand(opts *cacheOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Evict: expired lineages and images, then least recently used under the watermarks",
		Long: `Run eviction over this worker's sandbox volume (docs/sandbox.md §Eviction),
the same pass the worker runs at start and, watermarks alone, after each
promotion:

  1. every repo-scoped lineage not restored within the expiry, and every
     image subvolume not booted within it, is deleted whatever the free
     space;
  2. when free space on the volume — statfs, the truth — is below the high
     watermark, repo-scoped lineages and images are deleted least recently
     used first until it is above the low watermark, with the volume synced
     and free space re-read after each deletion.

Global lineages are bounded by their caps and never evicted. A run in
flight is never affected: its copies are snapshots of their own. Each
deletion is printed with its reason. contrib/systemd/mill-cache-prune.timer
runs this daily.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := cacheStore(cmd, opts)
			if err != nil {
				return err
			}
			policy, err := cachePolicy(cmd, opts, store)
			if err != nil {
				return err
			}
			return runCachePrune(cmd, store, policy)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.expiry, "expiry", "30d", "delete a repo lineage not restored for longer than this, such as 30d")
	f.StringVar(&opts.imageExpiry, "image-expiry", "", "delete an image not booted for longer than this (default: the expiry)")
	f.StringVar(&opts.highWatermark, "high-watermark", cache.DefaultHighWatermark.String(), "free space below which least recently used lineages and images go: a share of the volume such as 10%, or a size such as 50G")
	f.StringVar(&opts.lowWatermark, "low-watermark", cache.DefaultLowWatermark.String(), "free space at which eviction stops, above the high watermark")
	f.StringVar(&opts.machinesDir, "machines-dir", machine.DefaultMachines, "directory the image subvolumes live in")
	f.StringVar(&opts.sudo, "sudo", defaultImageSudo, "how the image directory, which is root's, is written: the sudo command, or none")
	return cmd
}

// cachePolicy is the eviction the flags and the file describe, over the
// image store the same flags describe.
func cachePolicy(cmd *cobra.Command, opts *cacheOptions, store *cache.Store) (cache.Policy, error) {
	config, err := LoadConfig(opts.config)
	if err != nil {
		return cache.Policy{}, err
	}
	for name, target := range map[string]*string{
		"expiry": &opts.expiry, "image-expiry": &opts.imageExpiry,
		"high-watermark": &opts.highWatermark, "low-watermark": &opts.lowWatermark,
		"machines-dir": &opts.machinesDir,
	} {
		value := map[string]string{
			"expiry": config.CacheExpiry, "image-expiry": config.ImageExpiry,
			"high-watermark": config.CacheHighWatermark, "low-watermark": config.CacheLowWatermark,
			"machines-dir": config.MachinesDir,
		}[name]
		if value != "" && unset(cmd, name) {
			*target = value
		}
	}
	policy := store.Policy(nil)
	if policy.Expiry, err = parseWindow(opts.expiry); err != nil {
		return cache.Policy{}, fmt.Errorf("--expiry: %q is not a window such as 720h or 30d", opts.expiry)
	}
	if opts.imageExpiry != "" {
		if policy.ImageExpiry, err = parseWindow(opts.imageExpiry); err != nil {
			return cache.Policy{}, fmt.Errorf("--image-expiry: %q is not a window such as 720h or 30d", opts.imageExpiry)
		}
	}
	if policy.High, err = cache.ParseWatermark(opts.highWatermark); err != nil {
		return cache.Policy{}, fmt.Errorf("--high-watermark: %w", err)
	}
	if policy.Low, err = cache.ParseWatermark(opts.lowWatermark); err != nil {
		return cache.Policy{}, fmt.Errorf("--low-watermark: %w", err)
	}
	sudo := opts.sudo
	if sudo == "none" {
		sudo = ""
	}
	policy.Images = image.New(driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: sudo}),
		image.Config{Root: opts.sandboxRoot, Machines: opts.machinesDir})
	return policy, nil
}

func runCachePrune(cmd *cobra.Command, store *cache.Store, policy cache.Policy) error {
	report, err := store.Evict(cmd.Context(), policy)
	printEviction(cmd.OutOrStdout(), store.LineageRoot(), report)
	if err != nil {
		return fmt.Errorf("cache prune: %w", err)
	}
	return nil
}

// printEviction is the report as `cache prune` and the worker's start show
// it: a line per deletion with its reason, then free space against the
// watermarks.
func printEviction(out io.Writer, root string, report cache.Report) {
	if len(report.Deleted) == 0 {
		_, _ = fmt.Fprintf(out, "nothing to evict under %s\n", root)
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "KIND\tNAME\tLAST USED\tPASS\tREASON")
		for _, d := range report.Deleted {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.Kind, evictedName(d), restoredText(d.LastUsed), d.Pass, d.Reason)
		}
		_ = w.Flush()
	}
	if report.Filesystem.Total > 0 {
		_, _ = fmt.Fprintf(out, "free %s of %s (high watermark %s, low %s)%s\n",
			cache.FormatSize(report.Free), cache.FormatSize(report.Filesystem.Total),
			cache.FormatSize(report.High), cache.FormatSize(report.Low), shortText(report))
	}
}

// evictedName is the lineage's `<scope>/<name>` with its generations, or
// the image's short digest.
func evictedName(d cache.Deletion) string {
	if d.Kind == cache.KindImage {
		return d.Image.Digest.Short()
	}
	return d.Name() + " gen-" + generationsText(d.Generations)
}

func shortText(report cache.Report) string {
	if report.Short {
		return "; nothing left to evict and free space is still below the low watermark"
	}
	return ""
}

func newCacheLsCommand(opts *cacheOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the cache lineages this worker holds",
		Long: `List every lineage under the sandbox root: scope, name, the generations on
disk, the latest generation's du -s estimate at promotion, when a run last
restored it, and a flag — "over-cap" when the latest estimate exceeds the
scope's cap, "reset" when the last promotion emptied a global lineage for
exceeding it, "evicted" when eviction emptied a repo lineage. The repo
cap is advisory: the flag is the whole of it. Global lineages first, then by repository and name.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := cacheStore(cmd, opts)
			if err != nil {
				return err
			}
			return runCacheLs(cmd, store)
		},
	}
}

func runCacheLs(cmd *cobra.Command, store *cache.Store) error {
	summaries, err := store.List()
	if err != nil {
		return fmt.Errorf("cache ls: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(summaries) == 0 {
		_, _ = fmt.Fprintf(out, "no lineages under %s\n", store.LineageRoot())
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SCOPE\tNAME\tGENERATIONS\tLATEST SIZE\tLAST RESTORED\tFLAGS")
	for _, s := range summaries {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Lineage.Scope, s.Lineage.Name, generationsText(s.Generations),
			latestSizeText(s), restoredText(s.LastRestoredAt), flagsText(s))
	}
	return w.Flush()
}

// generationsText is the generation numbers, `-` for none.
func generationsText(numbers []int) string {
	if len(numbers) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(numbers))
	for _, n := range numbers {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ",")
}

// latestSizeText is the latest generation's estimate, `-` for none or for a
// generation the manifest never recorded.
func latestSizeText(s cache.Summary) string {
	if s.Latest == nil || s.Latest.SizeBytes == 0 {
		return "-"
	}
	return cache.FormatSize(s.Latest.SizeBytes)
}

func restoredText(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.UTC().Format(time.RFC3339)
}

// flagsText is what an operator should look at: `over-cap`, `reset`, or
// `-`.
func flagsText(s cache.Summary) string {
	var flags []string
	if s.OverCap {
		flags = append(flags, "over-cap")
	}
	if s.Reset != nil && len(s.Generations) == 0 {
		flags = append(flags, "reset")
	}
	if s.Evicted != nil && len(s.Generations) == 0 {
		flags = append(flags, "evicted")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}

// cacheStore is the store the flags describe, over the systemd driver as
// the caller's own uid: nothing about a lineage needs root. The
// configuration file fills in what the command line left alone.
func cacheStore(cmd *cobra.Command, opts *cacheOptions) (*cache.Store, error) {
	if err := checkNamedConfig(cmd, opts.config); err != nil {
		return nil, err
	}
	config, err := LoadConfig(opts.config)
	if err != nil {
		return nil, err
	}
	if err := resolveSandboxRoot(cmd, &opts.sandboxRoot, config.SandboxRoot); err != nil {
		return nil, err
	}
	if config.CacheGlobalCap != "" && unset(cmd, "global-cap") {
		opts.globalCap = config.CacheGlobalCap
	}
	if config.CacheRepoCap != "" && unset(cmd, "repo-cap") {
		opts.repoCap = config.CacheRepoCap
	}
	globalCap, err := cache.ParseSize(opts.globalCap)
	if err != nil {
		return nil, fmt.Errorf("--global-cap: %w", err)
	}
	repoCap, err := cache.ParseSize(opts.repoCap)
	if err != nil {
		return nil, fmt.Errorf("--repo-cap: %w", err)
	}
	globalCaps, err := parseGlobalCaps(config.CacheGlobalCaps)
	if err != nil {
		return nil, err
	}
	host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
	return cache.New(host, cache.Config{Root: opts.sandboxRoot, GlobalCap: globalCap, GlobalCaps: globalCaps, RepoCap: repoCap}), nil
}
