package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateAddsTimeoutToPreTimeoutTasksTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre-timeout.db")
	oldSchema := strings.Replace(
		schemaSQL,
		"    timeout      INTEGER,            -- task timeout in seconds, nullable (0 = no timeout)\n",
		"",
		1,
	)
	if oldSchema == schemaSQL {
		t.Fatal("test setup did not remove tasks.timeout from the old schema")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		db.Close()
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO projects (name, config_json) VALUES ('project', '{}');
		INSERT INTO jobs (id, project_name, config_json, status, total_tasks, target, created_at)
		VALUES ('job', 'project', '{}', 'running', 1, 'local', 1);
		INSERT INTO tasks (
			id, job_id, project_name, command, params_json,
			gpus_needed, status, target, enqueued_at
		) VALUES ('task', 'job', 'project', 'true', '{}', 1, 'pending', 'local', 1);
	`); err != nil {
		db.Close()
		t.Fatalf("seed old database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old database: %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open and migrate old database: %v", err)
	}
	defer s.Close()

	task, err := s.GetTask(context.Background(), "task")
	if err != nil {
		t.Fatalf("GetTask after migration: %v", err)
	}
	if task == nil || task.ID != "task" || task.Timeout != 0 {
		t.Fatalf("GetTask after migration = %#v, want task with zero timeout", task)
	}

	tasks, err := s.ListTasks(context.Background(), TaskFilter{JobID: "job"})
	if err != nil {
		t.Fatalf("ListTasks after migration: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task" {
		t.Fatalf("ListTasks after migration = %#v, want the legacy task", tasks)
	}
}
