package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// The file capabilities `podman export` drops and the exported tree gets
// back, without which rootless podman inside the machine cannot write its
// uid map (docs/spikes/nspawn-per-run-sandbox.md E2).
var restoredCapabilities = [...]struct{ capability, path string }{
	{"cap_setuid=ep", "usr/bin/newuidmap"},
	{"cap_setgid=ep", "usr/bin/newgidmap"},
}

// Finding is a derived build that did not produce an image: what was asked,
// where it failed, and what the tools said. It is attached to the run's
// first session and never fails the run (docs/sandbox.md §"Derived
// images", §"Preparatory steps").
type Finding struct {
	// Digest is the derived digest that was not built.
	Digest Digest
	// Base is what the run boots instead.
	Base Digest
	// Packages is the list that failed.
	Packages []string
	// Nested is whether the nested variant was being built.
	Nested bool
	// Stage is where it failed: `build` for podman build and dnf, `export`
	// for the subvolume, the extraction, the capabilities or the rename.
	Stage string
	// Output is the last [TailLines] lines of the build, podman's own
	// closing line dropped so that they are dnf's; or the export error.
	Output string
	// Err is what the driver returned.
	Err error
}

func (f *Finding) String() string {
	var b strings.Builder
	variant := ""
	if f.Nested {
		variant = " (nested variant)"
	}
	fmt.Fprintf(&b, "the derived image for packages [%s]%s failed at %s (%v); the run boots from base %s.",
		strings.Join(f.Packages, ", "), variant, f.Stage, f.Err, f.Base.Short())
	if f.Output != "" {
		b.WriteString("\ndnf said:\n")
		b.WriteString(f.Output)
	}
	return b.String()
}

// Ensure is sequence step 5: the subvolume for base plus packages — and
// the [Nested] variant when a variant names it — built if it is not there,
// under one lock per digest so that two runs of a repository build it once
// and the second waits and finds it. What comes back is the digest to boot
// — the derived one, or the base's when the derived build failed, with the
// [Finding] that says why. The base subvolume is ensured the same way when
// the fallback needs it; when the base itself cannot be built there is
// nothing to boot and that is the error.
func (s *Store) Ensure(ctx context.Context, base Digest, packages []string, variants ...Variant) (Digest, *Finding, error) {
	nested := hasNested(variants)
	packages = Packages(packages)
	if nested {
		packages = Packages(append(packages, NestedPackages...))
	}
	digest := Derived(base, packages, variants...)

	finding, err := s.ensureLocked(ctx, base, digest, packages, nested)
	if err != nil {
		return "", nil, err
	}
	if finding == nil {
		return digest, nil, nil
	}
	if digest == base {
		return "", finding, fmt.Errorf("%w: the base %s could not be exported: %s", ErrNoBase, base.Short(), finding)
	}
	if _, _, err := s.Ensure(ctx, base, nil); err != nil {
		return "", finding, err
	}
	return base, finding, nil
}

// ensureLocked takes the digest's lock, re-checks for the subvolume, and
// builds it when it is missing. The finding is nil when the subvolume is
// there by the end.
func (s *Store) ensureLocked(ctx context.Context, base, digest Digest, packages []string, nested bool) (*Finding, error) {
	unlock, err := s.lock(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer unlock()

	present, err := s.host.Exists(ctx, s.Path(digest))
	if err != nil {
		return nil, err
	}
	if present {
		return nil, nil
	}
	return s.buildOnHostOutsideAnyMachine(ctx, base, digest, packages, nested), nil
}

// buildOnHostOutsideAnyMachine is the derived build: `podman build` on the
// host, outside any machine, from a generated Containerfile; the result
// exported into `mill-image-<digest>.building` as a subvolume, the two
// file capabilities restored, the manifest written, and the subvolume
// renamed into place. It is the one place the worker runs a container
// outside a machine, which docs/sandbox.md §"Derived images" asks to be
// visible; the name is that.
//
// Every failure is a [Finding] and leaves nothing behind: no `.building`,
// no export container, no built image.
func (s *Store) buildOnHostOutsideAnyMachine(ctx context.Context, base, digest Digest, packages []string, nested bool) *Finding {
	fail := func(stage, output string, err error) *Finding {
		return &Finding{Digest: digest, Base: base, Packages: packages, Nested: nested, Stage: stage, Output: output, Err: err}
	}

	budget, cancel := context.WithTimeout(ctx, s.config.BuildBudget)
	defer cancel()

	contextDir, err := os.MkdirTemp("", "mill-image-*")
	if err != nil {
		return fail("build", "", err)
	}
	defer func() { _ = os.RemoveAll(contextDir) }()
	containerfile := filepath.Join(contextDir, "Containerfile")
	if err := os.WriteFile(containerfile, []byte(s.Containerfile(base, packages, nested)), 0o600); err != nil {
		return fail("build", "", err)
	}

	tag := builtTag(digest)
	output, err := s.host.BuildImageOnHost(budget, driver.ImageBuild{
		Context:       contextDir,
		Containerfile: containerfile,
		Tags:          []string{tag},
	})
	if err != nil {
		if errors.Is(budget.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w after the %s budget", err, s.config.BuildBudget)
		}
		return fail("build", Tail(output), err)
	}
	defer func() { _ = s.host.RemoveImage(context.WithoutCancel(ctx), tag) }()

	building := s.building(digest)
	if err := s.host.CreateSubvolume(budget, building); err != nil {
		return fail("export", "", err)
	}
	cleanup := func() { _ = s.host.DeleteSubvolume(context.WithoutCancel(ctx), building, false) }

	if err := s.host.ExportImage(budget, tag, building); err != nil {
		cleanup()
		return fail("export", "", err)
	}
	for _, restore := range restoredCapabilities {
		if err := s.host.Setcap(budget, restore.capability, filepath.Join(building, restore.path)); err != nil {
			cleanup()
			return fail("export", "", err)
		}
	}
	if err := s.writeManifest(Manifest{Digest: digest, Base: base, Packages: packages, Nested: nested, BuiltAt: s.config.Now()}); err != nil {
		cleanup()
		return fail("export", "", err)
	}
	if err := s.host.Rename(budget, building, s.Path(digest)); err != nil {
		cleanup()
		_ = os.Remove(s.manifestPath(digest))
		return fail("export", "", err)
	}
	return nil
}

// Containerfile is the derived image's recipe: the base by digest, one dnf
// transaction when there are packages, and for the nested variant the
// sudoers line that gives `agent` everything. It is what the build is
// given.
func (s *Store) Containerfile(base Digest, packages []string, nested bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", s.BaseReference(base))
	if len(packages) > 0 {
		fmt.Fprintf(&b, "RUN dnf install -y %s && dnf clean all\n", strings.Join(packages, " "))
	}
	if nested {
		fmt.Fprintf(&b, "RUN echo 'agent ALL=(ALL) NOPASSWD: ALL' > %[1]s && chmod 0440 %[1]s && visudo -cf %[1]s\n", NestedSudoers)
	}
	return b.String()
}

// builtTag names the built image in the host's container storage until its
// tree is exported.
func builtTag(digest Digest) string {
	return "localhost/mill/derived:" + string(digest)
}

// Tail is a build transcript's last [TailLines] lines with podman's own
// closing line dropped, so what remains is what dnf said
// (docs/spikes/nspawn-per-run-sandbox.md E10).
func Tail(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "Error: building at STEP") {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > TailLines {
		kept = kept[len(kept)-TailLines:]
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// lock takes `<root>/images/<digest>.lock` with flock, waiting for a holder
// to finish; the returned function releases it. The descriptor is opened
// close-on-exec, as every file Go opens is, so podman and its network
// helper never inherit it — the leak the spike saw from `flock <file> cmd`
// (docs/spikes/nspawn-per-run-sandbox.md E10).
func (s *Store) lock(ctx context.Context, digest Digest) (func(), error) {
	if err := os.MkdirAll(s.dir(), 0o750); err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	path := filepath.Join(s.dir(), string(digest)+lockSuffix)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640) //nolint:gosec // the lock file is the worker's own
	if err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	locked := make(chan error, 1)
	go func() { locked <- syscall.Flock(int(file.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-locked:
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("image: lock %s: %w", path, err)
		}
		return func() { _ = file.Close() }, nil
	case <-ctx.Done():
		// flock cannot be interrupted; the lock, once granted, is released
		// by the close that follows it.
		go func() {
			<-locked
			_ = file.Close()
		}()
		return nil, fmt.Errorf("image: waiting for the lock on %s: %w", path, ctx.Err())
	}
}
