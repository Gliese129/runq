package dashboard

// Tests for GET/PUT /api/v1/ui (RQ2-1 §D): opaque blob semantics, the {}
// default, the 64KB cap, and atomic replacement. Harness pattern follows
// config_edit_test.go: RUNQ_DATA_DIR → t.TempDir().

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

func newUIStateHarness(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	server := NewServer(backend.NewUnavailableBackend(errors.New("unused")), &config.GlobalConfig{})
	return server, dir
}

func doUI(server *Server, method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/ui", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestUIStateGetMissingFileReturnsEmptyObject(t *testing.T) {
	server, _ := newUIStateHarness(t)
	rec := doUI(server, "GET", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("body = %q, want {}", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestUIStatePutGetRoundtrip(t *testing.T) {
	server, dir := newUIStateHarness(t)
	doc := `{"groups":{"order":["a"],"map":{"j1":"a"}},"appearance":{"density":"compact"}}`
	rec := doUI(server, "PUT", doc)
	if rec.Code != 200 {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = doUI(server, "GET", "")
	if got := strings.TrimSpace(rec.Body.String()); got != doc {
		t.Errorf("roundtrip: got %q want %q", got, doc)
	}
	if _, err := os.Stat(filepath.Join(dir, "ui.json")); err != nil {
		t.Errorf("ui.json not written: %v", err)
	}
}

func TestUIStatePutReplacesWholeDocument(t *testing.T) {
	server, _ := newUIStateHarness(t)
	doUI(server, "PUT", `{"a":1,"b":2}`)
	doUI(server, "PUT", `{"c":3}`)
	rec := doUI(server, "GET", "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"c":3}` {
		t.Errorf("whole-document replace violated: %q", got)
	}
}

func TestUIStatePutRejectsInvalidJSON(t *testing.T) {
	server, _ := newUIStateHarness(t)
	if rec := doUI(server, "PUT", `{broken`); rec.Code != 400 {
		t.Errorf("invalid JSON: status = %d", rec.Code)
	}
	if rec := doUI(server, "PUT", `[1,2,3]`); rec.Code != 400 {
		t.Errorf("non-object JSON: status = %d", rec.Code)
	}
	// Neither attempt may have created the file.
	rec := doUI(server, "GET", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("rejected PUT left state: %q", got)
	}
}

func TestUIStatePutRejectsOversize(t *testing.T) {
	server, _ := newUIStateHarness(t)
	big := `{"pad":"` + strings.Repeat("x", maxUIStateBytes) + `"}`
	if rec := doUI(server, "PUT", big); rec.Code != 413 {
		t.Errorf("oversize: status = %d, want 413", rec.Code)
	}
}

func TestUIStateLegalNonObjectFileServesEmpty(t *testing.T) {
	// Codex r1 finding 4: a hand-edited file holding a LEGAL JSON array
	// passes json.Valid but violates the "GET always yields an object"
	// contract — it must degrade to {} exactly like a corrupt file.
	server, dir := newUIStateHarness(t)
	for _, content := range []string{`[1,2,3]`, `"scalar"`, `42`} {
		if err := os.WriteFile(filepath.Join(dir, "ui.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doUI(server, "GET", "")
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
			t.Errorf("file %q: status=%d body=%q, want 200 {}", content, rec.Code, rec.Body.String())
		}
	}
}

func TestUIStateCorruptFileServesEmpty(t *testing.T) {
	server, dir := newUIStateHarness(t)
	if err := os.WriteFile(filepath.Join(dir, "ui.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := doUI(server, "GET", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("corrupt file: status=%d body=%q", rec.Code, rec.Body.String())
	}
	// The next PUT heals the file.
	doUI(server, "PUT", `{"ok":true}`)
	rec = doUI(server, "GET", "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"ok":true}` {
		t.Errorf("heal failed: %q", got)
	}
}

func TestUIStateNoTempFileLeftBehind(t *testing.T) {
	server, dir := newUIStateHarness(t)
	doUI(server, "PUT", `{"a":1}`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ui.json.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
