package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Base is the base image as one provision resolved it.
type Base struct {
	// Digest is the base's manifest digest.
	Digest Digest
	// Reference is what was asked of the registry, or what the pin named.
	Reference string
	// Pinned is a digest from worker configuration: the registry was not
	// consulted.
	Pinned bool
	// Fallback is the last digest this worker resolved, used because the
	// registry did not answer this time; Reason says how it failed. The
	// provision records it.
	Fallback bool
	// Reason is why the registry was not the source, when it was not.
	Reason string
}

// baseRecordName is the worker's record of the last base it resolved,
// under its image directory.
const baseRecordName = "base.json"

// baseRecord is what the record holds.
type baseRecord struct {
	Digest     Digest    `json:"digest"`
	Reference  string    `json:"reference"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// ResolveBase is sequence step 4 (docs/sandbox.md §"Sequence of a run"): the
// base tag pulled from Harbor and read for its digest, or the pin the
// configuration names. When Harbor does not answer, the last resolved
// digest stands in if its subvolume is on this host, and the result says
// so; when there is none, the error is [ErrNoBase].
func (s *Store) ResolveBase(ctx context.Context) (Base, error) {
	if digest, err := ParseDigest(s.config.Pin); err == nil {
		return Base{Digest: digest, Reference: s.BaseReference(digest), Pinned: true}, nil
	}
	tag := s.config.Pin
	if tag == "" {
		tag = DefaultTag
	}
	reference := s.config.Registry + "/" + s.config.Repository + ":" + tag

	digest, err := s.pull(ctx, reference)
	if err == nil {
		if err := s.writeBaseRecord(baseRecord{Digest: digest, Reference: reference, ResolvedAt: s.config.Now()}); err != nil {
			return Base{}, err
		}
		return Base{Digest: digest, Reference: reference}, nil
	}

	last, readErr := s.readBaseRecord()
	if readErr != nil {
		return Base{}, fmt.Errorf("%w: %s: %w; and no base was resolved before", ErrNoBase, reference, err)
	}
	present, existsErr := s.driver.Exists(ctx, s.Path(last.Digest))
	if existsErr != nil {
		return Base{}, existsErr
	}
	if !present {
		return Base{}, fmt.Errorf("%w: %s: %w; the last resolved base %s has no subvolume at %s",
			ErrNoBase, reference, err, last.Digest.Short(), s.Path(last.Digest))
	}
	return Base{
		Digest:    last.Digest,
		Reference: last.Reference,
		Fallback:  true,
		Reason:    fmt.Sprintf("%s could not be resolved (%v); using the base resolved at %s", reference, err, last.ResolvedAt.Format(time.RFC3339)),
	}, nil
}

// pull fetches the reference and reads the digest podman records for it.
func (s *Store) pull(ctx context.Context, reference string) (Digest, error) {
	if err := s.driver.PullImage(ctx, reference); err != nil {
		return "", err
	}
	raw, err := s.driver.ImageDigest(ctx, reference)
	if err != nil {
		return "", err
	}
	return ParseDigest(raw)
}

func (s *Store) baseRecordPath() string {
	return filepath.Join(s.dir(), baseRecordName)
}

func (s *Store) readBaseRecord() (baseRecord, error) {
	raw, err := os.ReadFile(s.baseRecordPath())
	if err != nil {
		return baseRecord{}, err
	}
	var record baseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return baseRecord{}, fmt.Errorf("image: %s: %w", s.baseRecordPath(), err)
	}
	if _, err := ParseDigest(string(record.Digest)); err != nil {
		return baseRecord{}, fmt.Errorf("image: %s: %w", s.baseRecordPath(), err)
	}
	return record, nil
}

func (s *Store) writeBaseRecord(record baseRecord) error {
	return writeJSON(s.baseRecordPath(), record)
}

// writeJSON writes a record atomically: a temporary file beside it, renamed
// over it, so a reader never sees half of one.
func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("image: %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("image: %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("image: %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("image: %w", err)
	}
	return nil
}

// isNotExist is a read of a record that is not there.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
