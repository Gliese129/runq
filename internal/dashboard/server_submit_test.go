package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
)

type submitCaptureBackend struct {
	*UnavailableBackend
	cfg  job.JobConfig
	opts SubmitOptions
}

func (b *submitCaptureBackend) SubmitJob(_ context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	b.cfg = cfg
	b.opts = opts
	return "job-test", 1, nil
}

type renameCaptureBackend struct {
	*UnavailableBackend
	oldName string
	newName string
}

func (b *renameCaptureBackend) RenameProject(_ context.Context, oldName, newName string) error {
	b.oldName = oldName
	b.newName = newName
	return nil
}

func TestHandleSubmitJobPropagatesPreflightOption(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantSkip bool
	}{
		{name: "default preflight", path: "/api/dashboard/jobs", wantSkip: false},
		{name: "skip preflight", path: "/api/dashboard/jobs?no_preflight=1", wantSkip: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &submitCaptureBackend{
				UnavailableBackend: NewUnavailableBackend(errors.New("unused")),
			}
			server := NewServer(backend, config.ModeDaemon, &config.GlobalConfig{})

			body := `{"project":"pytrain-example","fixed_params":{"epochs":30},"sweep":[]}`
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(body))
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
		UnavailableBackend: NewUnavailableBackend(errors.New("unused")),
	}
	server := NewServer(backend, config.ModeDaemon, &config.GlobalConfig{})

	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/projects/old-name/rename", strings.NewReader(`{"new_name":"new-name"}`))
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

func TestDaemonSubmitPathHonorsPreflightOption(t *testing.T) {
	if got := daemonSubmitPath(SubmitOptions{}); got != "/api/jobs" {
		t.Fatalf("default path = %q, want /api/jobs", got)
	}
	if got := daemonSubmitPath(SubmitOptions{SkipPreflight: true}); got != "/api/jobs?no_preflight=1" {
		t.Fatalf("skip path = %q, want /api/jobs?no_preflight=1", got)
	}
}
