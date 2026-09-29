package run

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steading-ai/mill/internal/workflow"
)

// crockford is the ULID alphabet a comment id's leading characters are
// encoded in — the same alphabet skift's own id minting uses
// (skift internal/domain/ulid.go), duplicated here because skift does not
// export it.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// comments is a node's unresolved comments, oldest first, ready for a
// prompt: skift's own order preserved, resolved comments left out here so
// workflow.Prompt never has to know skift's resolution model.
func (r *Runner) comments(ctx context.Context, ref string) ([]workflow.Comment, error) {
	all, err := r.options.Skift.Comments(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("comments for %s: %w", ref, err)
	}
	comments := make([]workflow.Comment, 0, len(all))
	for _, c := range all {
		if c.GetResolvedAt() != nil {
			continue
		}
		comments = append(comments, workflow.Comment{
			Ref:       c.GetId(),
			Body:      c.GetBody(),
			Author:    c.GetAuthor(),
			CreatedAt: commentCreatedAt(c.GetId()),
		})
	}
	return comments, nil
}

// commentCreatedAt decodes the millisecond timestamp a canonical ULID leads
// with — skift.Comment carries no creation time of its own, and Id is
// documented as "ULID assigned by the server" (skift.pb.go). A ULID's first
// 10 characters are exactly its 48-bit timestamp (plus the encoding's own 2
// leading padding bits), decoded here the reverse of how skift's newULID
// (and mill's own internal/store/ulid.go, the same layout) encodes it. Best
// effort: an id that is not a valid ULID prefix (too short, or a character
// outside the Crockford alphabet) yields the zero time rather than an
// error, since a comment still renders correctly without one.
func commentCreatedAt(id string) time.Time {
	if len(id) < 10 {
		return time.Time{}
	}
	var ms uint64
	for _, ch := range id[:10] {
		idx := strings.IndexRune(crockford, ch)
		if idx < 0 {
			return time.Time{}
		}
		ms = ms<<5 | uint64(idx)
	}
	return time.UnixMilli(int64(ms)).UTC()
}
