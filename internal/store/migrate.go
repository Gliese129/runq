package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// Migrate executes schema.sql against the database, then runs idempotent
// ALTER TABLE steps for columns that were added to existing tables in later
// versions (CREATE TABLE IF NOT EXISTS does NOT add columns to pre-existing
// tables, so each new column needs explicit handling here).
//
// All steps are safe to run repeatedly.
func (s *Store) Migrate() error {
	ctx, cancel := defaultCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	return s.addMissingColumns(ctx)
}

// addMissingColumns is the home for "tables already existed in an older schema,
// add this column" steps. Each addColumnIfMissing call is a no-op if the column
// is already present, so this is safe across daemon upgrades.
func (s *Store) addMissingColumns(ctx context.Context) error {
	// L2-C: task_dir holds the per-task workspace at <working_dir>/.runq/<task_id>.
	// Without this, daemon restart can't locate the metrics.jsonl from reclaimed tasks.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "task_dir", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.task_dir: %w", err)
	}
	// L2-E: external_id holds the HPC scheduler job id (sbatch/qsub) so refresh
	// can map a task back to its cluster job. Empty for daemon-managed tasks.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "external_id", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.external_id: %w", err)
	}
	return nil
}

// addColumnIfMissing inspects the table via PRAGMA table_info and only runs
// ALTER TABLE ... ADD COLUMN when the column is absent. SQLite's ALTER TABLE
// only supports a small subset of changes; ADD COLUMN with a simple type is fine.
//
// Cannot use IF NOT EXISTS in ALTER TABLE prior to SQLite 3.35; the pragma
// pre-check works on every version.
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, colType string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctypeStr  string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctypeStr, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colType)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	return nil
}
