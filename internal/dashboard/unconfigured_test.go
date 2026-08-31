package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/store"
)

func newUnconfiguredServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	multi, err := backend.NewMultiBackend(map[string]backend.Backend{}, st, "")
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(multi, &config.GlobalConfig{})
}

func TestUnconfiguredBootstrapAndHealth(t *testing.T) {
	server := newUnconfiguredServer(t)

	configRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(configRec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if configRec.Code != http.StatusOK {
		t.Fatalf("config status %d, body %s", configRec.Code, configRec.Body.String())
	}
	var cfg backend.ConfigResponse
	if err := json.Unmarshal(configRec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetState != backend.TargetStateUnconfigured || cfg.DefaultTarget != "" || len(cfg.Targets) != 0 {
		t.Fatalf("config = %+v, want explicit unconfigured state", cfg)
	}

	healthRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if healthRec.Code != http.StatusOK {
		t.Fatalf("health status %d, body %s", healthRec.Code, healthRec.Body.String())
	}
	var health struct {
		TargetState string                 `json:"target_state"`
		Targets     []backend.TargetHealth `json:"targets"`
	}
	if err := json.Unmarshal(healthRec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.TargetState != backend.TargetStateUnconfigured || len(health.Targets) != 0 {
		t.Fatalf("health = %+v, want explicit unconfigured state", health)
	}

	targetsRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(targetsRec, httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil))
	if targetsRec.Code != http.StatusOK {
		t.Fatalf("targets status %d, body %s", targetsRec.Code, targetsRec.Body.String())
	}
	var targets targetsListResponse
	if err := json.Unmarshal(targetsRec.Body.Bytes(), &targets); err != nil {
		t.Fatal(err)
	}
	if targets.Items == nil || len(targets.Items) != 0 {
		t.Fatalf("targets items = %#v, want non-nil empty list", targets.Items)
	}
}

func TestUnconfiguredSubmitIsActionableConflict(t *testing.T) {
	server := newUnconfiguredServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"config":{"project":"demo"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var response backend.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != backend.CodeInvalidState || !strings.Contains(response.Error, "runq target add") {
		t.Fatalf("error response = %+v, want actionable invalid_state", response)
	}
}
