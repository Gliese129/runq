package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

const targetsCfg = `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: sbatch {{run_sh}}
`

// newConfigEditHarness points RUNQ_DATA_DIR at a temp config.yaml and
// returns a Server plus a counter of reconciler notifications.
func newConfigEditHarness(t *testing.T, cfgYAML string) (*Server, *int) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(backend.NewUnavailableBackend(errors.New("unused")), &config.GlobalConfig{})
	notified := 0
	server.SetConfigChanged(func() { notified++ })
	return server, &notified
}

func currentGeneration(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Generation
}

func doJSON(server *Server, method, path, ifMatch, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPutTargetIfMatch(t *testing.T) {
	server, notified := newConfigEditHarness(t, targetsCfg)
	gen := currentGeneration(t)
	body := `{"scheduler":"pbs","submit_template":"qsub {{run_sh}}"}`

	// Fresh If-Match (quoted, ETag-style): lands, reconciler notified.
	rec := doJSON(server, http.MethodPut, "/api/v1/targets/b", `"`+gen+`"`, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh If-Match: status %d, body %s", rec.Code, rec.Body.String())
	}
	if *notified != 1 {
		t.Fatalf("reconciler notified %d times, want 1", *notified)
	}
	newGen := currentGeneration(t)
	if newGen == gen {
		t.Fatal("generation did not advance")
	}

	// Stale If-Match: 409 + generation_conflict + current_generation.
	rec = doJSON(server, http.MethodPut, "/api/v1/targets/c", gen, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale If-Match: status %d, body %s", rec.Code, rec.Body.String())
	}
	var er backend.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	if er.Code != backend.CodeGenerationConflict {
		t.Fatalf("code = %q, want %q", er.Code, backend.CodeGenerationConflict)
	}
	if er.CurrentGeneration != newGen {
		t.Fatalf("current_generation = %q, want %q", er.CurrentGeneration, newGen)
	}
	if *notified != 1 {
		t.Fatalf("conflict must not notify the reconciler (notified=%d)", *notified)
	}

	// No If-Match: unconditional write still works (legacy clients).
	rec = doJSON(server, http.MethodPut, "/api/v1/targets/c", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("unconditional: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteTargetIfMatch(t *testing.T) {
	server, _ := newConfigEditHarness(t, targetsCfg)
	gen := currentGeneration(t)

	// Stale If-Match first (any different value): 409, target survives.
	rec := doJSON(server, http.MethodDelete, "/api/v1/targets/a", "definitely-stale", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale delete: status %d, body %s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) != 1 {
		t.Fatalf("stale delete removed the target anyway: %+v", cfg.Targets)
	}

	// Fresh If-Match: deletes — including the LAST target (regression:
	// the old map-overlay save silently kept it).
	rec = doJSON(server, http.MethodDelete, "/api/v1/targets/a", gen, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh delete: status %d, body %s", rec.Code, rec.Body.String())
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) != 0 {
		t.Fatalf("last target not deleted: %+v", cfg.Targets)
	}
}

func TestPutGlobalConfigIfMatch(t *testing.T) {
	server, notified := newConfigEditHarness(t, targetsCfg)
	gen := currentGeneration(t)

	rec := doJSON(server, http.MethodPut, "/api/v1/config", gen, `{"data_path":"/data/runq"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh: status %d, body %s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPath != "/data/runq" {
		t.Fatalf("data_path = %q, want /data/runq", cfg.DataPath)
	}
	if *notified != 1 {
		t.Fatalf("reconciler notified %d times, want 1", *notified)
	}

	rec = doJSON(server, http.MethodPut, "/api/v1/config", gen, `{"default_target":"a"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestPutGlobalConfigClearAndOmit(t *testing.T) {
	server, _ := newConfigEditHarness(t, "data_path: /old\n"+targetsCfg)

	// Omitted field: unchanged (review fix #3 — pointer payload).
	rec := doJSON(server, http.MethodPut, "/api/v1/config", currentGeneration(t), `{"default_target":"a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("omit: status %d, body %s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPath != "/old" {
		t.Fatalf("omitted data_path was changed: %q", cfg.DataPath)
	}

	// Present-but-empty: CLEARS the key on disk (was a silent 200 no-op).
	rec = doJSON(server, http.MethodPut, "/api/v1/config", currentGeneration(t), `{"data_path":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d, body %s", rec.Code, rec.Body.String())
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPath != "" {
		t.Fatalf("data_path not cleared: %q", cfg.DataPath)
	}
}

// Review fix #1: GET /config must describe the FILE as it is NOW, not the
// boot snapshot — the WebUI conflict dialog uses it as "the disk version".
func TestHandleConfigReadsDiskNotSnapshot(t *testing.T) {
	server, _ := newConfigEditHarness(t, "data_path: /disk-v2\n"+targetsCfg)
	// The harness constructs NewServer with an EMPTY snapshot cfg — if the
	// handler served the snapshot, data_path would come back "".

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp backend.ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DataPath != "/disk-v2" {
		t.Fatalf("data_path = %q, want /disk-v2 (served boot snapshot?)", resp.DataPath)
	}
	if resp.DefaultTarget != "a" {
		t.Fatalf("default_target = %q, want a", resp.DefaultTarget)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Name != "a" {
		t.Fatalf("targets = %+v, want [a]", resp.Targets)
	}
	if resp.ConfigGeneration == "" {
		t.Fatal("config_generation missing")
	}

	// Edit the file; the next GET must follow it with no restart.
	dir := os.Getenv("RUNQ_DATA_DIR")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("data_path: /disk-v3\n"+targetsCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	var resp2 backend.ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.DataPath != "/disk-v3" {
		t.Fatalf("after edit: data_path = %q, want /disk-v3", resp2.DataPath)
	}
	if resp2.ConfigGeneration == resp.ConfigGeneration {
		t.Fatal("generation did not follow the file edit")
	}
}
