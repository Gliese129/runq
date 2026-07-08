package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
)

type submitCaptureBackend struct {
	*backend.UnavailableBackend
	cfg  job.JobConfig
	opts backend.SubmitOptions
}

func (b *submitCaptureBackend) SubmitJob(_ context.Context, cfg job.JobConfig, opts backend.SubmitOptions) (string, int, error) {
	b.cfg = cfg
	b.opts = opts
	return "job-test", 1, nil
}

type renameCaptureBackend struct {
	*backend.UnavailableBackend
	oldName string
	newName string
}

func (b *renameCaptureBackend) RenameProject(_ context.Context, oldName, newName string) error {
	b.oldName = oldName
	b.newName = newName
	return nil
}

func TestHandleSubmitJobPropagatesPreflightOption(t *testing.T) {
	// v1: options ride the BODY (D12) — {config, target?, skip_preflight?}.
	tests := []struct {
		name     string
		body     string
		wantSkip bool
	}{
		{name: "default preflight",
			body:     `{"config":{"project":"pytrain-example","fixed_params":{"epochs":30},"sweep":[]}}`,
			wantSkip: false},
		{name: "skip preflight",
			body:     `{"config":{"project":"pytrain-example","fixed_params":{"epochs":30},"sweep":[]},"skip_preflight":true}`,
			wantSkip: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &submitCaptureBackend{
				UnavailableBackend: backend.NewUnavailableBackend(errors.New("unused")),
			}
			server := NewServer(backend, &config.GlobalConfig{})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if backend.cfg.Project != "pytrain-example" {
				t.Fatalf("project = %q, want pytrain-example", backend.cfg.Project)
			}
			if backend.opts.SkipPreflight != tt.wantSkip {
				t.Fatalf("SkipPreflight = %v, want %v", backend.opts.SkipPreflight, tt.wantSkip)
			}
		})
	}
}

func TestHandleRenameProjectRoutesToBackend(t *testing.T) {
	backend := &renameCaptureBackend{
		UnavailableBackend: backend.NewUnavailableBackend(errors.New("unused")),
	}
	server := NewServer(backend, &config.GlobalConfig{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/old-name/rename", strings.NewReader(`{"new_name":"new-name"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if backend.oldName != "old-name" || backend.newName != "new-name" {
		t.Fatalf("rename = (%q, %q), want (old-name, new-name)", backend.oldName, backend.newName)
	}
}
