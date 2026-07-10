package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
	"gopkg.in/yaml.v3"
	"path"
)

// Registry manages the set of registered projects, backed by SQLite.
type Registry struct {
	db    *sql.DB
	fsFor func(target string) rfs.FS // nil = everything local
}

// NewRegistry creates a Registry using the given database connection.
// WithFSRouter injects the target→filesystem router (RQ-65): a project's
// yaml lives on its home target's filesystem, and every read/write of it
// must go there. nil router (or nil result) = local filesystem.
func (r *Registry) WithFSRouter(fn func(target string) rfs.FS) *Registry {
	r.fsFor = fn
	return r
}

// fs resolves the filesystem owning a project with the given home target.
func (r *Registry) fs(target string) rfs.FS {
	if r.fsFor != nil {
		if fsys := r.fsFor(target); fsys != nil {
			return fsys
		}
	}
	return rfs.NewLocalFS()
}

func NewRegistry(db *sql.DB) *Registry {
	return &Registry{db: db}
}

// Add registers a new project. Writes project.yaml to working_dir then
// inserts into the database. The directory must exist and be writable —
// project.yaml is the source of truth, so a failed write blocks registration.
// Returns an error if a project with the same name already exists.
func (r *Registry) Add(ctx context.Context, cfg Config) error {
	var existing int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE name = ?`, cfg.ProjectName).Scan(&existing)
	if err == nil {
		return fmt.Errorf("project %q already exists", cfg.ProjectName)
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("query project %q: %w", cfg.ProjectName, err)
	}

	// Write project.yaml — hard failure (file is source of truth)
	if err := cfg.WriteYAML(r.fs(cfg.Target)); err != nil {
		return fmt.Errorf("write project.yaml: %w", err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO projects (name, config_json) VALUES (?, ?)`,
		cfg.ProjectName, string(data),
	)
	if err != nil {
		// SQLite UNIQUE constraint violation contains "UNIQUE constraint failed"
		return fmt.Errorf("project %q already exists or database error: %w", cfg.ProjectName, err)
	}
	return nil
}

// Get returns a project by name.
// Returns a descriptive error if the project does not exist.
func (r *Registry) Get(ctx context.Context, name string) (*Config, error) {
	var raw string
	err := r.db.QueryRowContext(ctx,
		`SELECT config_json FROM projects WHERE name = ?`, name,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("query project %q: %w", name, err)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal project %q: %w", name, err)
	}
	return r.syncFromYAML(ctx, name, &cfg), nil
}

// syncFromYAML reconciles the DB copy with <working_dir>/project.yaml on
// every Get. project.yaml is the source of truth (CLI users hand-edit it);
// the DB row is a cache for listing/joins. Drift → reload the file and
// refresh the cache, so CLI and GUI can never disagree about a project.
// Fault-tolerant: a missing or unparsable file falls back to the DB copy
// (a botched hand-edit must not make the project vanish).
func (r *Registry) syncFromYAML(ctx context.Context, name string, dbCfg *Config) *Config {
	if dbCfg.WorkingDir == "" {
		return dbCfg
	}
	// Read through the project's HOME filesystem (RQ-65) — for a remote
	// project this reuses the lane's warm SSH connection. Fault-tolerant
	// as ever: unreachable target = fall back to the DB cache.
	buf, err := r.fs(dbCfg.Target).ReadFile(path.Join(dbCfg.WorkingDir, "project.yaml"))
	if err != nil {
		return dbCfg
	}
	var fileCfg Config
	if yaml.Unmarshal(buf, &fileCfg) != nil {
		return dbCfg
	}
	// Sanity: yaml.Unmarshal happily produces a zero struct for garbage
	// scalars. A real project.yaml always carries these two fields.
	if fileCfg.WorkingDir == "" || fileCfg.CmdTemplate == "" {
		return dbCfg
	}
	// Ownership: the file speaks ONLY for the project it names. Multiple
	// projects can legitimately share a working_dir (--project-file
	// submissions); syncing the dir's project.yaml into a DIFFERENTLY-named
	// registry entry would graft one project's config onto another's
	// identity. Same-name (or legacy nameless) files stay source of truth.
	if fileCfg.ProjectName != "" && fileCfg.ProjectName != name {
		return dbCfg
	}
	// Identity stays with the DB key — renames go through Rename(), not a
	// hand-edited project_name (that would orphan the job FKs).
	fileCfg.ProjectName = name

	fileJSON, err1 := json.Marshal(fileCfg)
	dbJSON, err2 := json.Marshal(dbCfg)
	if err1 != nil || err2 != nil || string(fileJSON) == string(dbJSON) {
		return dbCfg
	}
	// File wins: refresh the cache. Best-effort — a read-only DB still
	// returns the file's truth for this call.
	_, _ = r.db.ExecContext(ctx, `UPDATE projects SET config_json = ? WHERE name = ?`, string(fileJSON), name)
	return &fileCfg
}

// List returns all registered projects, ordered by name.
func (r *Registry) List(ctx context.Context) ([]Config, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT config_json FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var configs []Config
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("unmarshal project: %w", err)
		}
		configs = append(configs, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return configs, nil
}

// Update modifies an existing project's config.
// Rewrites project.yaml and updates the database.
// Returns an error if the project does not exist.
func (r *Registry) Update(ctx context.Context, cfg Config) error {
	var existing int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE name = ?`, cfg.ProjectName).Scan(&existing)
	if err == sql.ErrNoRows {
		return fmt.Errorf("project %q not found", cfg.ProjectName)
	}
	if err != nil {
		return fmt.Errorf("query project %q: %w", cfg.ProjectName, err)
	}

	// Rewrite project.yaml (explicit update) — on the project's home FS.
	if err := cfg.OverwriteYAML(r.fs(cfg.Target)); err != nil {
		return fmt.Errorf("write project.yaml: %w", err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	result, err := r.db.ExecContext(ctx,
		`UPDATE projects SET config_json = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
		string(data), cfg.ProjectName,
	)
	if err != nil {
		return fmt.Errorf("update project %q: %w", cfg.ProjectName, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", cfg.ProjectName)
	}
	return nil
}

// Match returns all projects whose WorkingDir is equal to or a parent of dir.
// This is used to auto-detect which project(s) a script belongs to.
func (r *Registry) Match(ctx context.Context, dir string) ([]Config, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	var matched []Config
	for _, cfg := range all {
		wd := filepath.Clean(cfg.WorkingDir)
		if wd == "" {
			continue
		}
		if dir == wd || strings.HasPrefix(dir, wd+string(filepath.Separator)) {
			matched = append(matched, cfg)
		}
	}
	return matched, nil
}

// Rename atomically renames a project from oldName to newName.
// Updates all references in jobs and tasks within a transaction,
// then rewrites project.yaml with the new name.
func (r *Registry) Rename(ctx context.Context, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	// Check new name doesn't already exist
	var existing int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE name = ?`, newName).Scan(&existing); err == nil {
		return fmt.Errorf("project %q already exists", newName)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get old config
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT config_json FROM projects WHERE name = ?`, oldName).Scan(&raw); err != nil {
		return fmt.Errorf("project %q not found", oldName)
	}

	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return fmt.Errorf("unmarshal project %q: %w", oldName, err)
	}

	cfg.ProjectName = newName
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}

	// Insert new, update references, delete old
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects (name, config_json) VALUES (?, ?)`, newName, string(data)); err != nil {
		return fmt.Errorf("insert renamed project: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET project_name = ? WHERE project_name = ?`, newName, oldName); err != nil {
		return fmt.Errorf("update jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET project_name = ? WHERE project_name = ?`, newName, oldName); err != nil {
		return fmt.Errorf("update tasks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, oldName); err != nil {
		return fmt.Errorf("delete old project: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rename: %w", err)
	}

	// Rewrite project.yaml with new name — on the project's home FS.
	_ = cfg.OverwriteYAML(r.fs(cfg.Target))

	return nil
}

// Remove deletes a project by name.
// Returns an error if the project does not exist.
func (r *Registry) Remove(ctx context.Context, name string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("remove project %q: %w", name, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", name)
	}
	return nil
}

// ── Archive ──
//
// Archive hides a project (and, in global listings, its jobs — the cascade
// is applied by the job queries) from default lists. The flag lives in its
// own column, NEVER in config_json: project.yaml is the truth for the
// experiment config, but "archived" is runq bookkeeping — a yaml re-sync
// must not resurrect or clobber it.

func (r *Registry) Archive(ctx context.Context, name string) error {
	// Guard: archiving cascades the project's jobs out of the global lists,
	// so live work must not be hidden — same rule as the per-job guard
	// (paused counts: it is resumable, hence forgettable).
	var active int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE project_name = ? AND status IN `+store.ActiveStatusesSQL(),
		name).Scan(&active)
	if err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("project %q has %d active job(s) — kill them (or let them finish) before archiving", name, active)
	}
	res, err := r.db.ExecContext(ctx, `UPDATE projects SET archived_at = ? WHERE name = ?`, time.Now().Unix(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("project %q not found", name)
	}
	return nil
}

func (r *Registry) Unarchive(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE projects SET archived_at = NULL WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("project %q not found", name)
	}
	return nil
}

// IsArchived reports whether a project is archived.
func (r *Registry) IsArchived(ctx context.Context, name string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE name = ? AND archived_at IS NOT NULL`, name).Scan(&n)
	return n > 0, err
}

// RequireSubmittable is THE precondition for landing new work in a project:
// it must exist and must not be archived (archived jobs cascade out of the
// default lists — a submit would run invisibly). FAIL-CLOSED throughout:
// an unreadable state is a refusal, never a pass. Every submit path
// (daemon service, HPC backend, and any future writer) calls this single
// gate so a new entry point can't forget the rule.
func (r *Registry) RequireSubmittable(ctx context.Context, name string) error {
	var archived sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT archived_at FROM projects WHERE name = ?`, name).Scan(&archived)
	if err == sql.ErrNoRows {
		return fmt.Errorf("project %q not found", name)
	}
	if err != nil {
		return fmt.Errorf("check project %q: %w", name, err)
	}
	if archived.Valid {
		return fmt.Errorf("project %q is archived — `runq project unarchive %s` before submitting", name, name)
	}
	return nil
}

// ArchivedNames returns the set of archived project names. Kept separate
// from List() (which returns Configs — yaml-truth material) so archived
// state rides alongside, not inside, the config.
func (r *Registry) ArchivedNames(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name FROM projects WHERE archived_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}
