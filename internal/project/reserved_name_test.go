package project

import (
	"context"
	"strings"
	"testing"
)

// The dashboard's projects namespace reserves "new" (/projects/new is the
// creation route; vue-router ranks static segments above params, so a
// project literally named "new" has no URL). Both naming entrances must
// refuse it — a frontend comment is not a contract (Codex RQ2-3 r1 F5).
// Renaming AWAY from a legacy reserved name stays possible: only the NEW
// name is validated, which is the migration path for projects that predate
// the rule.
func TestReservedProjectNames(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	err := r.Add(ctx, Config{ProjectName: "new", WorkingDir: t.TempDir(), CmdTemplate: "python x.py {{args}}"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Add(new) = %v, want reserved-name error", err)
	}

	if err := r.Add(ctx, Config{ProjectName: "ok", WorkingDir: t.TempDir(), CmdTemplate: "python x.py {{args}}"}); err != nil {
		t.Fatalf("Add(ok): %v", err)
	}
	err = r.Rename(ctx, "ok", "new")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Rename(ok→new) = %v, want reserved-name error", err)
	}
}
