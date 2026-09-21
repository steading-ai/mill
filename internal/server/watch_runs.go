package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// DefaultWatchRunsWindow is how far back a WatchRuns cursor may be before it
// is refused with FailedPrecondition, unless the service was given
// WithWatchRunsWindow.
const DefaultWatchRunsWindow = 24 * time.Hour

// DefaultWatchRunsKeepalive is how often an otherwise-idle WatchRuns stream
// sends a cursor-only message, so a proxy does not close it, unless the
// service was given WithWatchRunsKeepalive.
const DefaultWatchRunsKeepalive = 30 * time.Second

// watchRunsCursorSep joins the two halves of a WatchRuns cursor: an
// RFC3339Nano timestamp, which contains neither a "|" nor anything an event
// id (an idgen id) does.
const watchRunsCursorSep = "|"

// encodeWatchRunsCursor is the wire form of the (occurred_at, id) pair
// events_occurred_at_id_idx (00024_events_watch_index.sql) orders by —
// nanosecond precision, so two events in the same second never collide on
// reconnect.
func encodeWatchRunsCursor(at time.Time, id string) string {
	return at.Format(time.RFC3339Nano) + watchRunsCursorSep + id
}

// decodeWatchRunsCursor reverses encodeWatchRunsCursor.
func decodeWatchRunsCursor(cursor string) (time.Time, string, error) {
	at, id, ok := strings.Cut(cursor, watchRunsCursorSep)
	if !ok || id == "" {
		return time.Time{}, "", fmt.Errorf("watch runs: malformed cursor %q: not one this server issued; reconnect with no cursor", cursor)
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("watch runs: malformed cursor %q: not one this server issued; reconnect with no cursor: %w", cursor, err)
	}
	return t, id, nil
}

// WatchRuns streams the ledger's events table from a cursor: every run, not
// one. The first message of a fresh subscription (an empty cursor) carries
// the opening position alone, no event, so a caller that reads it before
// doing anything else never misses an event landing between opening the
// stream and acting on what it saw. A cursor older than the configured
// replay window is refused with FailedPrecondition naming the window.
// docs/ledger.md §events.
func (s *StoreService) WatchRuns(ctx context.Context, req *connect.Request[millv1.WatchRunsRequest], stream *connect.ServerStream[millv1.WatchRunsResponse]) error {
	cursor := req.Msg.GetCursor()
	at, id, err := s.watchRunsOpen(ctx, cursor)
	if err != nil {
		return err
	}
	project := req.Msg.GetProject()

	if cursor == "" {
		if err := stream.Send(&millv1.WatchRunsResponse{Cursor: encodeWatchRunsCursor(at, id)}); err != nil {
			return err
		}
	}
	lastSent := time.Now()

	for {
		// Subscribed before this wake's read, so a signal landing between the
		// read below and the Wait at the bottom of the loop is not missed
		// (Observer.Subscribe's own contract). The empty key is the one a
		// stream over every run watches; see logsFollowEveryRun.
		woken := s.observer.Subscribe("")

		events, err := s.svc.WatchableEvents(ctx, at, id, project)
		if err != nil {
			return connectError(err)
		}
		for _, e := range events {
			at, id = e.Event.GetOccurredAt().AsTime(), e.ID
			if err := stream.Send(&millv1.WatchRunsResponse{Cursor: encodeWatchRunsCursor(at, id), Event: e.Event}); err != nil {
				return err
			}
			lastSent = time.Now()
		}

		if len(events) == 0 && time.Since(lastSent) >= s.watchRunsKeepalive {
			if err := stream.Send(&millv1.WatchRunsResponse{Cursor: encodeWatchRunsCursor(at, id)}); err != nil {
				return err
			}
			lastSent = time.Now()
		}

		s.observer.Wait(ctx, woken)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// watchRunsOpen resolves cursor to a starting (occurred_at, id) position. An
// empty cursor opens at LatestEventCursor — the newest event already on
// record, read from the database's own clock rather than the app server's,
// so a fresh subscription's opening position never races app-server/
// database clock skew. A presented cursor is decoded and checked against
// the service's replay window, refused with FailedPrecondition naming the
// window when it is older.
func (s *StoreService) watchRunsOpen(ctx context.Context, cursor string) (time.Time, string, error) {
	if cursor == "" {
		at, id, err := s.svc.LatestEventCursor(ctx)
		if err != nil {
			return time.Time{}, "", connectError(err)
		}
		return at, id, nil
	}
	at, id, err := decodeWatchRunsCursor(cursor)
	if err != nil {
		return time.Time{}, "", connect.NewError(connect.CodeInvalidArgument, err)
	}
	if time.Since(at) > s.watchRunsWindow {
		return time.Time{}, "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("watch runs: cursor is older than the %s replay window; reconnect with no cursor to resume from the current tail (dropping events in between)", s.watchRunsWindow))
	}
	return at, id, nil
}
