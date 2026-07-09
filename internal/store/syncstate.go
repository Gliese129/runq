package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SyncState is the L4 freshness row for one (target, resource): when the
// sync loop last tried, last succeeded, and what went wrong if it didn't.
// This is what makes envelope refreshed_at/stale REAL instead of computed
// at response time (which was a lie).
type SyncState struct {
	Target      string
	Resource    string // "contact" | "tasks"
	LastSuccess int64  // unix; 0 = never
	LastAttempt int64  // unix; 0 = never
	LastError   string // "" = last attempt succeeded
}

// RecordSyncOutcome upserts one sync attempt's outcome. On success the
// row's last_success advances with last_attempt; on failure last_success
// is PRESERVED — the old photo's timestamp stays true even when new
// photos fail to develop.
func (s *Store) RecordSyncOutcome(ctx context.Context, target, resource string, outcome error) error {
	now := time.Now().Unix()
	errStr := ""
	if outcome != nil {
		errStr = outcome.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_state (target, resource, last_success, last_attempt, last_error)
		VALUES (?, ?, CASE WHEN ? = '' THEN ? ELSE 0 END, ?, ?)
		ON CONFLICT(target, resource) DO UPDATE SET
			last_attempt = excluded.last_attempt,
			last_error   = excluded.last_error,
			last_success = CASE WHEN excluded.last_error = ''
				THEN excluded.last_attempt ELSE sync_state.last_success END`,
		target, resource, errStr, now, now, errStr)
	return err
}

// GetSyncState returns the freshness row, or nil when this (target,
// resource) has never been synced (distinct from "synced with error").
func (s *Store) GetSyncState(ctx context.Context, target, resource string) (*SyncState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT last_success, last_attempt, last_error
		FROM sync_state WHERE target = ? AND resource = ?`, target, resource)
	st := SyncState{Target: target, Resource: resource}
	if err := row.Scan(&st.LastSuccess, &st.LastAttempt, &st.LastError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &st, nil
}
