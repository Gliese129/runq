package project

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/gliese129/runq/internal/store"
	_ "modernc.org/sqlite"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY, config_json TEXT)`); err != nil {
		t.Fatal(err)
	}
	return NewRegistry(db)
}

// project.yaml is the source of truth: a hand-edit is picked up on the next
// Get, and the DB cache refreshes. A broken file falls back to the DB copy.
func TestGetSyncsFromYAML(t *testing.T) {
	dir := t.TempDir()
	r := newTestRegistry(t)
	if err := r.Add(Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "python a.py {{args}}"}); err != nil {
		t.Fatal(err)
	}

	// Hand-edit the yaml (CLI persona).
	yamlPath := filepath.Join(dir, "project.yaml")
	edited := "project_name: p\nworking_dir: " + dir + "\ncommand_template: python b.py {{args}}\n"
	if err := os.WriteFile(yamlPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := r.Get("p")
	if err != nil {
		t.Fatal(err)
	}
	if got.CmdTemplate != "python b.py {{args}}" {
		t.Fatalf("yaml edit not picked up: %q", got.CmdTemplate)
	}

	// Cache refreshed: remove the file — Get must now serve the synced copy.
	os.Remove(yamlPath)
	got, _ = r.Get("p")
	if got.CmdTemplate != "python b.py {{args}}" {
		t.Fatalf("DB cache not refreshed: %q", got.CmdTemplate)
	}

	// Broken yaml → fall back to DB, project must not vanish.
	os.WriteFile(yamlPath, []byte("::not yaml::"), 0o644)
	got, err = r.Get("p")
	if err != nil || got.CmdTemplate != "python b.py {{args}}" {
		t.Fatalf("broken yaml must fall back to DB: %v %+v", err, got)
	}

	// Hand-edited project_name must NOT change identity (FK safety).
	os.WriteFile(yamlPath, []byte("project_name: renamed\nworking_dir: "+dir+"\ncommand_template: python c.py {{args}}\n"), 0o644)
	got, _ = r.Get("p")
	if got.ProjectName != "p" || got.CmdTemplate != "python c.py {{args}}" {
		t.Fatalf("identity must stay with DB key: %+v", got)
	}
}

// Archive guard: a project with live work must not be hidden — its jobs
// would cascade out of the global lists while still holding queue slots.
// paused counts as active (resumable = forgettable).
func TestProjectArchiveRefusesActiveJobs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := NewRegistry(st.DB())
	cfg := Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo {{x}}"}
	if err := reg.Add(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO jobs (id, project_name, description, config_json, status, created_at) VALUES ('jb1','p','','{}','paused',1)`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Archive("p"); err == nil {
		t.Fatal("archiving a project with a paused job must be refused")
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET status = 'done' WHERE id = 'jb1'`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Archive("p"); err != nil {
		t.Fatalf("archive with only terminal jobs: %v", err)
	}
	names, _ := reg.ArchivedNames()
	if !names["p"] {
		t.Fatal("ArchivedNames must report p")
	}
	if err := reg.Unarchive("p"); err != nil {
		t.Fatal(err)
	}
}
