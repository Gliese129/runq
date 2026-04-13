package project

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Registry manages the set of registered projects, backed by SQLite.
type Registry struct {
	db *sql.DB
}

// NewRegistry creates a Registry using the given database connection.
func NewRegistry(db *sql.DB) *Registry {
	return &Registry{db: db}
}

// Add registers a new project. Returns an error if a project with the
// same name already exists.
func (r *Registry) Add(cfg Config) error {
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
// Returns an error if the project does not exist.
func (r *Registry) Update(cfg Config) error {
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
