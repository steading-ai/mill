package cli

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// `mill cache` is the operator's side of docs/sandbox.md §Caching: which
// lineages this worker holds, how many generations each has, what the
// latest is estimated at, when a run last restored it, and whether it is
// over its cap. The listing is read from the directory tree and each
// lineage's manifest.json, never from btrfs, which needs root to list
// subvolumes.

// cacheOptions are the flags the cache verbs share, and the configuration
// keys behind them.
type cacheOptions struct {
	config      string
	sandboxRoot string
	globalCap   string
	repoCap     string
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
is over its scope's cap.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	f := cmd.PersistentFlags()
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml)")
	f.StringVar(&opts.sandboxRoot, "sandbox-root", machine.DefaultRoot, "the sandbox root; lineages live under <root>/cache")
	f.StringVar(&opts.globalCap, "global-cap", formatSize(cache.DefaultGlobalCap), "size estimate past which a global lineage is reset at promotion, such as 20G")
	f.StringVar(&opts.repoCap, "repo-cap", formatSize(cache.DefaultRepoCap), "size estimate past which a repo-scoped lineage is flagged, such as 10G")
	cmd.AddCommand(newCacheLsCommand(&opts))
	return cmd
}

func newCacheLsCommand(opts *cacheOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the cache lineages this worker holds",
		Long: `List every lineage under the sandbox root: scope, name, the generations on
disk, the latest generation's du -s estimate at promotion, when a run last
restored it, and a flag — "over-cap" when the latest estimate exceeds the
scope's cap, "reset" when the last promotion emptied a global lineage for
exceeding it. Global lineages first, then by repository and name.`,
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
	return formatSize(s.Latest.SizeBytes)
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
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}

// formatSize spells bytes the way `du -h` would: the largest binary unit
// that keeps the number above one, one decimal.
func formatSize(bytes uint64) string {
	units := []string{"B", "K", "M", "G", "T"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d%s", bytes, units[unit])
	}
	if value == float64(uint64(value)) {
		return fmt.Sprintf("%d%s", uint64(value), units[unit])
	}
	return fmt.Sprintf("%.1f%s", value, units[unit])
}

// parseSize reads a size such as `20G`, `512M`, `1.5T` or a bare number of
// bytes, in binary units.
func parseSize(text string) (uint64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	number := strings.TrimRight(text, "BKMGTbkmgt")
	suffix := strings.ToUpper(text[len(number):])
	suffix = strings.TrimSuffix(suffix, "B")
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}
	n, ok := shift[suffix]
	if !ok {
		return 0, fmt.Errorf("%q is not a size", text)
	}
	return uint64(value * float64(uint64(1)<<n)), nil
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
	globalCap, err := parseSize(opts.globalCap)
	if err != nil {
		return nil, fmt.Errorf("--global-cap: %w", err)
	}
	repoCap, err := parseSize(opts.repoCap)
	if err != nil {
		return nil, fmt.Errorf("--repo-cap: %w", err)
	}
	host := driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
	return cache.New(host, cache.Config{Root: opts.sandboxRoot, GlobalCap: globalCap, RepoCap: repoCap}), nil
}
