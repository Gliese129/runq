package store

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"
)

// ── RQ-75: retired target generations ──────────────────────────────────────
//
// When a target's config changes (or the target is removed) while tasks are
// still in flight on the OLD endpoint/templates, the old lane generation
// keeps running ("retiring") until its unfinished-task count reaches zero —
// the Kubernetes old-ReplicaSet shape. These rows survive daemon restarts
// (config snapshot = rebuild source) and feed the archived view in CLI/WebUI.

// LaneScope is a lane's OWNERSHIP predicate (RQ-75 review follow-up): the
// isolatable object every lane-side task query filters by, so active and
// retiring lanes can never probe, read or settle each other's tasks.
//
//	retiring lane: rows stamped with exactly its generation
//	active lane:   its generation, legacy '' rows, and ORPHAN generations
//	               (no live retirement record) — which the lane then adopts
//	               by restamping (ownership is written, never re-inferred)
//
// Retiring flips at rotation time while sensors are running — atomic.
type LaneScope struct {
	Target     string
	Generation string
	retiring   atomic.Bool
}

// NewLaneScope builds a scope; retiring lanes call MarkRetiring.
func NewLaneScope(target, generation string) *LaneScope {
	return &LaneScope{Target: target, Generation: generation}
}

// MarkRetiring narrows the scope to exactly this generation.
func (s *LaneScope) MarkRetiring() { s.retiring.Store(true) }

// ResumeActive widens the scope back — used when a retiring lane is
// PROMOTED to active again (config changed back to its generation).
func (s *LaneScope) ResumeActive() { s.retiring.Store(false) }

// IsRetiring reports the current scope mode.
func (s *LaneScope) IsRetiring() bool { return s.retiring.Load() }

// whereClause renders the ownership predicate (target column is handled by
// the caller's filter; this covers generation ownership only).
func (s *LaneScope) whereClause() (string, []any) {
	if s.IsRetiring() {
		return "COALESCE(target_generation,'') = ?", []any{s.Generation}
	}
	return `(COALESCE(target_generation,'') = ? OR COALESCE(target_generation,'') = ''
		OR target_generation NOT IN (
			SELECT generation FROM target_generations WHERE target = ? AND done_at IS NULL))`,
		[]any{s.Generation, s.Target}
}

// TargetGenerationRow is one retired (possibly still-retiring) generation.
type TargetGenerationRow struct {
	Target     string
	Generation string
	ConfigJSON string // TargetConfig snapshot, rebuild source
	Reason     string // 'changed' | 'removed'
	RetiredAt  int64  // unix
	DoneAt     *int64 // nil = still retiring
}

// UpsertRetiredGeneration records a generation entering retirement. Idempotent
// (re-retiring the same generation refreshes the snapshot, keeps retired_at).
func (s *Store) UpsertRetiredGeneration(ctx context.Context, g *TargetGenerationRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO target_generations (target, generation, config_json, reason, retired_at, done_at)
		VALUES (?, ?, ?, ?, ?, NULL)
		ON CONFLICT(target, generation) DO UPDATE SET
			config_json = excluded.config_json,
			reason      = excluded.reason,
			done_at     = NULL`,
		g.Target, g.Generation, g.ConfigJSON, g.Reason, g.RetiredAt)
	return err
}

// MarkGenerationDone closes out a retirement (unfinished count hit zero).
func (s *Store) MarkGenerationDone(ctx context.Context, target, generation string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE target_generations SET done_at = ? WHERE target = ? AND generation = ? AND done_at IS NULL`,
		time.Now().Unix(), target, generation)
	return err
}

// ListRetiringGenerations returns generations still tracking tasks
// (done_at IS NULL) — the set of retiring lanes to run/rebuild.
func (s *Store) ListRetiringGenerations(ctx context.Context) ([]TargetGenerationRow, error) {
	return s.listGenerations(ctx, `WHERE done_at IS NULL`)
}

// ListAllGenerations returns every recorded generation, still-retiring
// first, newest first within each group — the archive view's data.
func (s *Store) ListAllGenerations(ctx context.Context) ([]TargetGenerationRow, error) {
	return s.listGenerations(ctx, `ORDER BY (done_at IS NULL) DESC, retired_at DESC`)
}

// ListArchivedGenerations returns ALL recorded generations for a target
// (retiring and done), newest first — the CLI/WebUI archive view.
func (s *Store) ListArchivedGenerations(ctx context.Context, target string) ([]TargetGenerationRow, error) {
	return s.listGenerations(ctx, `WHERE target = ? ORDER BY retired_at DESC`, target)
}

func (s *Store) listGenerations(ctx context.Context, where string, args ...any) ([]TargetGenerationRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT target, generation, config_json, reason, retired_at, done_at FROM target_generations `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TargetGenerationRow
	for rows.Next() {
		var g TargetGenerationRow
		var done sql.NullInt64
		if err := rows.Scan(&g.Target, &g.Generation, &g.ConfigJSON, &g.Reason, &g.RetiredAt, &done); err != nil {
			return nil, err
		}
		if done.Valid {
			g.DoneAt = &done.Int64
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// IsRetiringGeneration reports whether (target, generation) has a LIVE
// retirement record (done_at IS NULL) — i.e. another lane owns its rows.
func (s *Store) IsRetiringGeneration(ctx context.Context, target, generation string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM target_generations WHERE target = ? AND generation = ? AND done_at IS NULL`,
		target, generation).Scan(&n)
	return n > 0, err
}

// CountUnfinishedGenerationTasks counts the tasks a retiring generation is
// still responsible for. Zero means the lane may close and the generation
// be marked done.
func (s *Store) CountUnfinishedGenerationTasks(ctx context.Context, target, generation string) (int, error) {
	var n int
	// ALL non-terminal rows count (review round 4): pending rows leave
	// the generation via forwarding (rotation) or funnel-settle (removal),
	// and the sweep must not close the lane until that has happened.
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks
		WHERE target = ? AND target_generation = ?
		  AND status NOT IN ('success', 'failed', 'killed')`,
		target, generation).Scan(&n)
	return n, err
}

// RestampTask reassigns one task to a generation (confirmed cross-
// generation rerun: the new attempt belongs to the active lane).
func (s *Store) RestampTask(ctx context.Context, taskID, generation string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET target_generation = ? WHERE id = ?`, generation, taskID)
	return err
}

// RestampPendingTasks migrates a target's not-yet-submitted tasks to the new
// generation (same-name config change: pending work auto-follows the new
// config). Returns the number of migrated rows.
func (s *Store) RestampPendingTasks(ctx context.Context, target, newGeneration string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET target_generation = ?
		WHERE target = ? AND status = 'pending'
		  AND (external_id IS NULL OR external_id = '')`,
		newGeneration, target)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StopPendingTasks kills a removed target's not-yet-submitted tasks (they
// have no lane to ever run on), recording the reason verbatim so CLI/WebUI
// can say WHY. In-flight tasks are untouched — the retiring lane tracks
// them to their real outcome. Returns the stopped task ids.
func (s *Store) StopPendingTasks(ctx context.Context, target, reason string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM tasks
		WHERE target = ? AND status = 'pending'
		  AND (external_id IS NULL OR external_id = '')`, target)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	now := time.Now().Unix()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE tasks SET status = 'killed', status_source = 'runq',
				failure_detail = ?, finished_at = ?
			WHERE id = ? AND status = 'pending'`, reason, now, id); err != nil {
			return ids, err
		}
	}
	return ids, nil
}
