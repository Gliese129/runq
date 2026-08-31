package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateArchivesLegacyMetricsWithoutDataLoss(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-metrics.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		t.Fatalf("create current base schema: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE metrics (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			job_id  TEXT NOT NULL,
			key     TEXT NOT NULL,
			value   REAL,
			step    INTEGER,
			ts      INTEGER NOT NULL,
			PRIMARY KEY (task_id, key, step, ts)
		);
		CREATE INDEX idx_metrics_job_key ON metrics(job_id, key);
		CREATE INDEX idx_metrics_task_key ON metrics(task_id, key, step);
		INSERT INTO projects (name, config_json) VALUES ('project', '{}');
		INSERT INTO jobs (
			id, project_name, config_json, status, total_tasks, target, created_at
		) VALUES ('job', 'project', '{}', 'done', 1, 'local', 1);
		INSERT INTO tasks (
			id, job_id, project_name, command, params_json,
			gpus_needed, status, target, enqueued_at
		) VALUES ('task', 'job', 'project', 'true', '{}', 1, 'success', 'local', 1);
		INSERT INTO metrics (task_id, job_id, key, value, step, ts) VALUES
			('task', 'job', 'loss', 3.5, 1, 100),
			('task', 'job', 'loss', 1.25, 2, 200),
			('task', 'job', 'accuracy', 0.9, NULL, 210);
	`); err != nil {
		db.Close()
		t.Fatalf("seed legacy metrics: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open and migrate legacy database: %v", err)
	}
	defer s.Close()

	assertLegacyMetricsArchive(t, s.DB())
	if err := s.Migrate(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	assertLegacyMetricsArchive(t, s.DB())
}

func assertLegacyMetricsArchive(t *testing.T, db *sql.DB) {
	t.Helper()
	var active, archived, rows int
	if err := db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'metrics'
	)`).Scan(&active); err != nil {
		t.Fatalf("inspect active legacy table: %v", err)
	}
	if err := db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'metrics_legacy_v1'
	)`).Scan(&archived); err != nil {
		t.Fatalf("inspect legacy archive: %v", err)
	}
	if active != 0 || archived != 1 {
		t.Fatalf("legacy table state = active:%d archived:%d, want 0/1", active, archived)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM metrics_legacy_v1`).Scan(&rows); err != nil {
		t.Fatalf("count archived metrics: %v", err)
	}
	if rows != 3 {
		t.Fatalf("archived metric rows = %d, want 3", rows)
	}
	var value float64
	if err := db.QueryRow(`
		SELECT value FROM metrics_legacy_v1
		WHERE task_id = 'task' AND key = 'loss' AND step = 2 AND ts = 200
	`).Scan(&value); err != nil {
		t.Fatalf("read archived metric: %v", err)
	}
	if value != 1.25 {
		t.Fatalf("archived metric value = %v, want 1.25", value)
	}
}
