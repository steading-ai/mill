package cache

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// Eviction is docs/sandbox.md §Eviction past what a promotion does to its
// own lineage: an expiry pass, then a watermark pass. Expiry deletes every
// repo-scoped lineage and every image nothing has used within the expiry,
// whatever the free space. The watermark pass reads free space from
// statfs — the truth — and, when it is below the high watermark, deletes
// repo-scoped lineages and images least recently used first until it is
// above the low one, re-reading free space after each deletion and its
// sync. Global lineages are bounded by their caps and never evicted; a
// run's copies under `runs/<id>/cache` are never touched, and a copy
// whose lineage is evicted is a snapshot of its own (the spike's E9).

// Policy is one eviction's parameters, from the store's configuration
// through [Store.Policy] and then narrowed by the trigger.
type Policy struct {
	// Expiry is how long a lineage goes unrestored before the expiry pass
	// deletes it. Zero skips the pass, which is what the trigger after a
	// promotion does: the cheap check is the watermark alone.
	Expiry time.Duration
	// ImageExpiry is the same for images; zero is Expiry.
	ImageExpiry time.Duration
	// High and Low are the watermarks. A zero High skips the watermark
	// pass.
	High Watermark
	Low  Watermark
	// Images is the image store whose subvolumes are victims beside the
	// lineages; nil leaves images alone.
	Images Images
	// ImagesOnly leaves lineages alone: `mill image prune`.
	ImagesOnly bool
}

// WatermarkOnly is the policy with no expiry pass: the check after a
// promotion.
func (p Policy) WatermarkOnly() Policy {
	p.Expiry, p.ImageExpiry = 0, 0
	return p
}

// Images is what eviction asks of the image store: [*image.Store].
type Images interface {
	List(ctx context.Context) ([]image.Manifest, error)
	Remove(ctx context.Context, digest image.Digest, sync bool) error
}

// Policy is the store's configured policy over the given image store, both
// passes on.
func (s *Store) Policy(images Images) Policy {
	return Policy{
		Expiry: s.config.Expiry,
		High:   s.config.HighWatermark,
		Low:    s.config.LowWatermark,
		Images: images,
	}
}

// Pass names which pass deleted something.
type Pass string

// The passes.
const (
	PassExpiry    Pass = "expiry"
	PassWatermark Pass = "watermark"
)

// Kind names what was deleted.
type Kind string

// The kinds of victim.
const (
	KindLineage Kind = "lineage"
	KindImage   Kind = "image"
)

// Deletion is one thing eviction deleted, and why.
type Deletion struct {
	// Kind is a lineage or an image.
	Kind Kind
	// Lineage is the lineage, for one; Generations the numbers that went.
	Lineage     Lineage
	Generations []int
	// Image is the image's record, for one.
	Image image.Manifest
	// LastUsed is when it was last restored or booted.
	LastUsed time.Time
	// Pass is which pass took it.
	Pass Pass
	// Reason is one sentence: the expiry it was past, or the free space it
	// was taken for.
	Reason string
}

// Name is `<scope>/<name>` for a lineage, the digest for an image.
func (d Deletion) Name() string {
	if d.Kind == KindImage {
		return string(d.Image.Digest)
	}
	return d.Lineage.String()
}

// Report is what an eviction did.
type Report struct {
	// Deleted is every deletion, in order.
	Deleted []Deletion
	// Filesystem is the sandbox filesystem as the watermark pass first
	// read it; zero when the pass did not run.
	Filesystem driver.Filesystem
	// Free is free space when eviction finished, as the last read said.
	Free uint64
	// High and Low are the watermarks in bytes on that filesystem.
	High, Low uint64
	// Short says the watermark pass ran out of victims with free space
	// still below the low watermark.
	Short bool
}

// victim is one candidate for the watermark pass, and its last use.
type victim struct {
	deletion Deletion
	lineage  Lineage
	image    image.Manifest
}

// Evict runs the policy and reports what it deleted. An error stops the
// eviction where it is; what was deleted before it is in the report.
func (s *Store) Evict(ctx context.Context, policy Policy) (Report, error) {
	if policy.ImageExpiry == 0 {
		policy.ImageExpiry = policy.Expiry
	}
	var report Report
	if policy.Expiry > 0 || (policy.Images != nil && policy.ImageExpiry > 0) {
		if err := s.expire(ctx, policy, &report); err != nil {
			return report, err
		}
	}
	if policy.High.IsZero() {
		return report, nil
	}
	err := s.watermark(ctx, policy, &report)
	return report, err
}

// expire is the first pass: every repo-scoped lineage last used before the
// cutoff, then every image.
func (s *Store) expire(ctx context.Context, policy Policy, report *Report) error {
	now := s.config.Now()
	victims, err := s.victims(ctx, policy)
	if err != nil {
		return err
	}
	for _, v := range victims {
		expiry := policy.Expiry
		if v.deletion.Kind == KindImage {
			expiry = policy.ImageExpiry
		}
		if expiry <= 0 || !v.deletion.LastUsed.Before(now.Add(-expiry)) {
			continue
		}
		v.deletion.Pass = PassExpiry
		v.deletion.Reason = ExpiredReason(now.Sub(v.deletion.LastUsed), expiry)
		if err := s.delete(ctx, policy, &v, report); err != nil {
			return err
		}
	}
	return nil
}

// watermark is the second pass: nothing unless free space is below the
// high watermark, then least recently used first until it is above the
// low one, with the truth re-read after each deletion.
func (s *Store) watermark(ctx context.Context, policy Policy, report *Report) error {
	fs, err := s.host.Statfs(s.config.Root)
	if err != nil {
		return fmt.Errorf("cache: free space under %s: %w", s.config.Root, err)
	}
	report.Filesystem, report.Free = fs, fs.Free
	report.High, report.Low = policy.High.Of(fs), policy.Low.Of(fs)
	if report.Low < report.High {
		return fmt.Errorf("cache: the low watermark %s is below the high watermark %s; eviction would stop before it started", policy.Low, policy.High)
	}
	if fs.Free >= report.High {
		return nil
	}
	victims, err := s.victims(ctx, policy)
	if err != nil {
		return err
	}
	for _, v := range victims {
		if report.Free >= report.Low {
			return nil
		}
		v.deletion.Pass = PassWatermark
		v.deletion.Reason = fmt.Sprintf("free space %s below the high watermark %s (%s of %s); least recently used",
			FormatSize(report.Free), policy.High, FormatSize(report.High), FormatSize(fs.Total))
		if err := s.delete(ctx, policy, &v, report); err != nil {
			return err
		}
		fs, err = s.host.Statfs(s.config.Root)
		if err != nil {
			return fmt.Errorf("cache: free space under %s: %w", s.config.Root, err)
		}
		report.Free = fs.Free
	}
	report.Short = report.Free < report.Low
	return nil
}

// victims is every repo-scoped lineage with a generation and every image
// the policy admits, least recently used first. Global lineages are not
// among them (docs/sandbox.md §Eviction), and a lineage with nothing on
// disk has nothing to delete.
func (s *Store) victims(ctx context.Context, policy Policy) ([]victim, error) {
	var out []victim
	if !policy.ImagesOnly {
		summaries, err := s.List()
		if err != nil {
			return nil, err
		}
		for _, summary := range summaries {
			if summary.Lineage.Scope.IsGlobal() || len(summary.Generations) == 0 {
				continue
			}
			out = append(out, victim{
				deletion: Deletion{Kind: KindLineage, Lineage: summary.Lineage, Generations: summary.Generations, LastUsed: summary.LastUsed},
				lineage:  summary.Lineage,
			})
		}
	}
	if policy.Images != nil {
		manifests, err := policy.Images.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("cache: images: %w", err)
		}
		for _, manifest := range manifests {
			out = append(out, victim{
				deletion: Deletion{Kind: KindImage, Image: manifest, LastUsed: manifest.LastUsed()},
				image:    manifest,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].deletion.LastUsed.Before(out[j].deletion.LastUsed) })
	return out, nil
}

// delete takes one victim and notes it in the report. Every subvolume
// deletion is synced, so a free-space read that follows tells the truth.
func (s *Store) delete(ctx context.Context, policy Policy, v *victim, report *Report) error {
	var err error
	if v.deletion.Kind == KindImage {
		err = policy.Images.Remove(ctx, v.image.Digest, true)
	} else {
		v.deletion.Generations, err = s.evictLineage(ctx, v.lineage, v.deletion.Reason)
	}
	if err != nil {
		return fmt.Errorf("cache: evict %s: %w", v.deletion.Name(), err)
	}
	report.Deleted = append(report.Deleted, v.deletion)
	return nil
}

// evictLineage deletes every generation of a lineage under its lock and
// records the eviction in its manifest. The directory, the lock and the
// manifest stay: a run that restored the lineage promotes into it as a
// fresh one, at gen-1, and nothing waiting on the lock finds it gone.
func (s *Store) evictLineage(ctx context.Context, lineage Lineage, reason string) ([]int, error) {
	unlock, err := s.lock(ctx, lineage)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Read under the lock: a promotion may have added a generation since
	// the victims were ranked.
	manifest, err := s.Manifest(lineage)
	if err != nil {
		return nil, err
	}
	evicted := &Evicted{At: s.config.Now(), Reason: reason}
	for _, generation := range manifest.Generations {
		if err := s.deleteReadOnly(ctx, s.GenerationPath(lineage, generation.Number), true); err != nil {
			return evicted.Generations, fmt.Errorf("gen-%d: %w", generation.Number, err)
		}
		evicted.Generations = append(evicted.Generations, generation.Number)
	}
	manifest.Generations = nil
	manifest.Evicted = evicted
	return evicted.Generations, s.writeManifest(lineage, manifest)
}

// ExpiredReason is the expiry pass's sentence for something unused for
// idle against an expiry: what `--dry-run` prints for what it would take.
func ExpiredReason(idle, expiry time.Duration) string {
	return fmt.Sprintf("unused for %s, past the expiry of %s", days(idle), days(expiry))
}

// days spells a duration in whole days when it is at least one, else as
// Go does.
func days(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return d.String()
}
