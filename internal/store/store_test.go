package store

import (
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
