package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Reading the workspaces a host is holding, without opening one.
//
// `mill workspace show` and `mill workspace clean` are asked of a run the
// worker has let go of: the socket is down, the run is waiting, parked or
// finished, and the checkout is what is left. Attach would put that back —
// it checks the branch out, arranges the signing key and binds the socket —
// which is more than a question deserves and would collide with a worker
// that still holds the run. So the git commands run against the directory
// directly, through the same chokepoint every other process goes through.

// Info is one run's workspace as an operator sees it.
type Info struct {
	// RunID is the run the directory belongs to, taken from its name.
	RunID string
	// Dir is the run's workspace directory, which holds the checkout, the
	// socket and nothing a session can reach.
	Dir string
	// Checkout is the clone itself.
	Checkout string
	// Branch is the branch the checkout is on, empty when it is detached.
	Branch string
	// Head is the commit the checkout points at.
	Head string
	// Clean reports whether git sees no change at all in the tree.
	Clean bool
	// Changed is how many paths git reports, staged or not.
	Changed int
	// Socket is where the workspace's unix socket would be.
	Socket string
	// SocketPresent reports whether it is still bound, which is how a
	// workspace a worker is holding looks from outside.
	SocketPresent bool
	// Modified is when the checkout was last written to.
	Modified time.Time
	// Detail is why the git half could not be read, empty when it was. A
	// directory whose clone is broken still has a path worth printing.
	Detail string
}

// ErrNoWorkspace is a run with no directory under this root: it was worked
// on another host, or its workspace has already been destroyed.
var ErrNoWorkspace = errors.New("workspace: this host holds no workspace for that run")

// Inspector reads the workspaces under a root. It opens none of them: no
// socket is bound, no branch is checked out, and no signing key is made.
type Inspector struct {
	root string
	// w is a workspace value used for nothing but its git runner, which
	// needs the binary, the execer and the environment Options names.
	w *Workspace
}

// NewInspector returns an inspector over the root the options name.
func NewInspector(options Options) *Inspector {
	return &Inspector{root: options.Root, w: New(options)}
}

// Root is the directory the inspector reads.
func (i *Inspector) Root() string { return i.root }

// RunIDs is every run this host holds a workspace directory for, sorted. A
// root that does not exist holds none, which is not an error: a host that
// has never run anything is a host with nothing to clean.
func (i *Inspector) RunIDs() ([]string, error) {
	entries, err := os.ReadDir(i.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: read %s: %w", i.root, err)
	}
	var ids []string
	for _, entry := range entries {
		// The signing key lives beside the runs and is not one of them,
		// and neither is anything else a name that is not a run id.
		if !entry.IsDir() || !isRunID(entry.Name()) {
			continue
		}
		ids = append(ids, entry.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// isRunID reports whether a directory name is a run's. Run ids are canonical
// ULIDs, so length and alphabet decide it without a store to ask.
func isRunID(name string) bool {
	if len(name) != runIDLen {
		return false
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, r := range name {
		if !strings.ContainsRune(alphabet, r) {
			return false
		}
	}
	return true
}

// Inspect reads one run's workspace: where it is, what branch it is on,
// where that branch points, and whether anything is uncommitted.
func (i *Inspector) Inspect(ctx context.Context, runID string) (*Info, error) {
	dir := filepath.Join(i.root, runID)
	checkout := filepath.Join(dir, checkoutDir)
	stat, err := os.Stat(checkout)
	if err != nil {
		return nil, fmt.Errorf("workspace: %s: %w", runID, ErrNoWorkspace)
	}
	info := &Info{
		RunID:    runID,
		Dir:      dir,
		Checkout: checkout,
		Socket:   filepath.Join(dir, socketName),
		Modified: stat.ModTime(),
	}
	if _, err := os.Stat(info.Socket); err == nil {
		info.SocketPresent = true
	}
	if err := i.readGit(ctx, info); err != nil {
		info.Detail = err.Error()
	}
	return info, nil
}

// readGit fills in the branch, the head and whether the tree is clean.
func (i *Inspector) readGit(ctx context.Context, info *Info) error {
	branch, err := i.w.branchOf(ctx, info.Checkout)
	if err != nil {
		return err
	}
	info.Branch = branch
	head, err := i.w.git(ctx, info.Checkout, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	info.Head = strings.TrimSpace(head)
	status, err := i.w.git(ctx, info.Checkout, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	info.Clean = true
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		info.Clean = false
		info.Changed++
	}
	return nil
}

// Remove deletes a run's workspace directory: the checkout, the socket and
// the run's own furniture. The signing key lives above it and survives, as
// it does a Destroy.
func (i *Inspector) Remove(runID string) error {
	dir := filepath.Join(i.root, runID)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("workspace: %s: %w", runID, ErrNoWorkspace)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace: remove %s: %w", dir, err)
	}
	return nil
}
