package store

import (
	"context"
	"fmt"
	"testing"
)

func TestOpenMemory(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer s.Close()

	// Verify tables exist by querying sqlite_master
	var count int
	err = s.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('projects','jobs','tasks')`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query sqlite_master: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 tables, found %d", count)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	// Migrate was already called by Open; calling again should not error.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
}

func TestWALEnabled(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	var mode string
	err = s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	// :memory: databases may report "memory" instead of "wal" since WAL
	// requires a file. Accept both.
	if mode != "wal" && mode != "memory" {
		t.Errorf("expected journal_mode wal or memory, got %q", mode)
	}
}

func TestForeignKeysEnabled(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	var fk int
	err = s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if err != nil {
		t.Fatalf("failed to query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("expected foreign_keys=1, got %d", fk)
	}
}

func TestUnknownIsActiveStatus(t *testing.T) {
	if !IsActiveStatus("unknown") {
		t.Fatal("unknown must be active while an execution attempt may still exist")
	}
	if !IsActiveStatus("submitting") {
		t.Fatal("submitting must be active while an external effect may be in flight")
	}
}

func TestProjectJobStatus(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		want     string
		wantErr  bool
	}{
		{name: "empty", want: "done"},
		{name: "waiting", statuses: []string{"pending", "pending"}, want: "pending"},
		{name: "submitting", statuses: []string{"submitting", "pending"}, want: "running"},
		{name: "unknown", statuses: []string{"unknown"}, want: "running"},
		{name: "mixed progress", statuses: []string{"success", "pending"}, want: "running"},
		{name: "done", statuses: []string{"success", "success"}, want: "done"},
		{name: "killed", statuses: []string{"killed", "killed"}, want: "killed"},
		{name: "failed", statuses: []string{"failed", "killed"}, want: "failed"},
		{name: "partial", statuses: []string{"success", "failed"}, want: "partial"},
		{name: "corrupt", statuses: []string{"teleported"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := make([]TaskRow, len(tt.statuses))
			for i, status := range tt.statuses {
				rows[i] = TaskRow{ID: fmt.Sprintf("t%d", i), Status: status}
			}
			got, err := ProjectJobStatus(rows)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestArchiveJobGuardAndVisibility(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	mustExec := func(q string) {
		t.Helper()
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}'), ('q', '{}')`)
	mustExec(`INSERT INTO jobs (id, project_name, description, config_json, status, created_at) VALUES
		('jb1', 'p', '', '{}', 'running', 1),
		('jb2', 'p', '', '{}', 'done', 2),
		('jb3', 'q', '', '{}', 'done', 3)`)

	// guard: active job refuses
	if err := s.ArchiveJob(ctx, "jb1"); err == nil {
		t.Fatal("archiving a running job must be refused")
	}
	if err := s.ArchiveJob(ctx, "jb2"); err != nil {
		t.Fatalf("archive done job: %v", err)
	}

	vis, err := s.ListJobsVisible(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(vis) != 2 { // jb1 (active) + jb3; jb2 hidden
		t.Fatalf("visible: want 2, got %d", len(vis))
	}

	// cascade: archive project q → jb3 vanishes from the GLOBAL list...
	mustExec(`UPDATE projects SET archived_at = 1 WHERE name = 'q'`)
	vis, _ = s.ListJobsVisible(ctx, "", "")
	if len(vis) != 1 || vis[0].ID != "jb1" {
		t.Fatalf("cascade: want only jb1, got %v", vis)
	}
	// ...but stays visible inside its explicit project scope
	scoped, _ := s.ListJobsVisible(ctx, "q", "")
	if len(scoped) != 1 || scoped[0].ID != "jb3" {
		t.Fatalf("scoped: want jb3, got %v", scoped)
	}
	// archived listing shows only the explicitly archived job
	arch, _ := s.ListJobsArchived(ctx, "", "")
	if len(arch) != 1 || arch[0].ID != "jb2" {
		t.Fatalf("archived: want jb2 only, got %v", arch)
	}

	// reversible; ListJobs (version-scan path) always sees everything
	if err := s.UnarchiveJob(ctx, "jb2"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListJobs(ctx, "", "")
	if len(all) != 3 {
		t.Fatalf("ListJobs must always return all rows, got %d", len(all))
	}
}
