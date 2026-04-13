package store

import _ "embed"

//go:embed schema.sql
var schemaSQL string

// Migrate executes schema.sql against the database.
// All statements use CREATE TABLE/INDEX IF NOT EXISTS, so this is idempotent.
func (s *Store) Migrate() error {
	ctx, cancel := defaultCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, schemaSQL)
	return err
}
