package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// RemoveLeftoverBuilds deletes every `mill-image-<digest>.building`
// subvolume a build that never finished left under the machines directory,
// and returns their paths (docs/sandbox.md §Reconciliation, step 2).
//
// The machines directory is root's and not the worker's to list, so the
// digests asked about are the ones this worker's image directory holds a
// build lock for: every build takes one before it creates the `.building`
// subvolume, and the lock file outlives the build. A lock another process
// holds right now is a build in flight and is left alone; its `.building`
// is that build's, not a leftover.
func (s *Store) RemoveLeftoverBuilds(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("image: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, lockSuffix) {
			continue
		}
		digest, err := ParseDigest(strings.TrimSuffix(name, lockSuffix))
		if err != nil {
			continue
		}
		path, err := s.removeLeftoverBuild(ctx, digest)
		if err != nil {
			return removed, err
		}
		if path != "" {
			removed = append(removed, path)
		}
	}
	return removed, nil
}

// removeLeftoverBuild deletes one digest's `.building` if it exists and no
// build holds the digest's lock, and returns the path it deleted.
func (s *Store) removeLeftoverBuild(ctx context.Context, digest Digest) (string, error) {
	unlock, held, err := s.tryLock(digest)
	if err != nil {
		return "", err
	}
	if held {
		return "", nil
	}
	defer unlock()
	building := s.building(digest)
	present, err := s.host.Exists(ctx, building)
	if err != nil {
		return "", err
	}
	if !present {
		return "", nil
	}
	if err := s.host.DeleteSubvolume(ctx, building, false); err != nil {
		return "", fmt.Errorf("image: remove leftover %s: %w", building, err)
	}
	return building, nil
}

// lockSuffix is the build lock's file name after the digest.
const lockSuffix = ".lock"

// tryLock takes the digest's build lock without waiting. held reports that
// another process has it.
func (s *Store) tryLock(digest Digest) (unlock func(), held bool, err error) {
	path := filepath.Join(s.dir(), string(digest)+lockSuffix)
	file, err := os.OpenFile(path, os.O_RDWR, 0o640) //nolint:gosec // the lock file is the worker's own
	if err != nil {
		return nil, false, fmt.Errorf("image: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("image: lock %s: %w", path, err)
	}
	return func() { _ = file.Close() }, false, nil
}
