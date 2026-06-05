package project

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Registry manages the set of registered projects, backed by SQLite.
type Registry struct {
	db *sql.DB
}

// NewRegistry creates a Registry using the given database connection.
func NewRegistry(db *sql.DB) *Registry {
	return &Registry{db: db}
}

// Add registers a new project. Writes project.yaml to working_dir then
// inserts into the database. The directory must exist and be writable —
// project.yaml is the source of truth, so a failed write blocks registration.
// Returns an error if a project with the same name already exists.
func (r *Registry) Add(cfg Config) error {
	var existing int
	err := r.db.QueryRow(`SELECT 1 FROM projects WHERE name = ?`, cfg.ProjectName).Scan(&existing)
	if err == nil {
		return fmt.Errorf("project %q already exists", cfg.ProjectName)
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("query project %q: %w", cfg.ProjectName, err)
	}

	// Write project.yaml — hard failure (file is source of truth)
	if err := cfg.WriteYAML(); err != nil {
		return fmt.Errorf("write project.yaml: %w", err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	_, err = r.db.Exec(
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
func (r *Registry) Get(name string) (*Config, error) {
	var raw string
	err := r.db.QueryRow(
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
	return &cfg, nil
}

// List returns all registered projects, ordered by name.
func (r *Registry) List() ([]Config, error) {
	rows, err := r.db.Query(`SELECT config_json FROM projects ORDER BY name`)
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
func (r *Registry) Update(cfg Config) error {
	var existing int
	err := r.db.QueryRow(`SELECT 1 FROM projects WHERE name = ?`, cfg.ProjectName).Scan(&existing)
	if err == sql.ErrNoRows {
		return fmt.Errorf("project %q not found", cfg.ProjectName)
	}
	if err != nil {
		return fmt.Errorf("query project %q: %w", cfg.ProjectName, err)
	}

	// Rewrite project.yaml (explicit update)
	if err := cfg.OverwriteYAML(); err != nil {
		return fmt.Errorf("write project.yaml: %w", err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	result, err := r.db.Exec(
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
func (r *Registry) Match(dir string) ([]Config, error) {
	all, err := r.List()
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
func (r *Registry) Rename(oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	// Check new name doesn't already exist
	var existing int
	if err := r.db.QueryRow(`SELECT 1 FROM projects WHERE name = ?`, newName).Scan(&existing); err == nil {
		return fmt.Errorf("project %q already exists", newName)
	}

	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get old config
	var raw string
	if err := tx.QueryRow(`SELECT config_json FROM projects WHERE name = ?`, oldName).Scan(&raw); err != nil {
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
	if _, err := tx.Exec(`INSERT INTO projects (name, config_json) VALUES (?, ?)`, newName, string(data)); err != nil {
		return fmt.Errorf("insert renamed project: %w", err)
	}
	if _, err := tx.Exec(`UPDATE jobs SET project_name = ? WHERE project_name = ?`, newName, oldName); err != nil {
		return fmt.Errorf("update jobs: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET project_name = ? WHERE project_name = ?`, newName, oldName); err != nil {
		return fmt.Errorf("update tasks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE name = ?`, oldName); err != nil {
		return fmt.Errorf("delete old project: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rename: %w", err)
	}

	// Rewrite project.yaml with new name
	_ = cfg.OverwriteYAML()

	return nil
}

// Remove deletes a project by name.
// Returns an error if the project does not exist.
func (r *Registry) Remove(name string) error {
	result, err := r.db.Exec(`DELETE FROM projects WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("remove project %q: %w", name, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", name)
	}
	return nil
}
