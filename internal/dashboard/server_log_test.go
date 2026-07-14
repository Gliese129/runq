package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/logfile"
)

// logStubFollower emits one fixed page, then ends the stream.
type logStubFollower struct {
	page     *backend.LogPage
	sent     bool
	maxBytes int64
}

func (f *logStubFollower) Next(ctx context.Context) (*backend.LogPage, error) {
	if f.sent {
		return nil, io.EOF
	}
	f.sent = true
	return f.page, nil
}
func (f *logStubFollower) Close() error        { return nil }
func (f *logStubFollower) SetMaxBytes(n int64) { f.maxBytes = n }

// logCaptureBackend records what the log handlers asked the backend for.
type logCaptureBackend struct {
	*backend.UnavailableBackend
	follower     *logStubFollower
	followOffset int64
	pageReq      *logfile.PageRequest
	tailLines    int
	readOffset   int64
	readLines    int
}

func (b *logCaptureBackend) TaskLogFollow(_ context.Context, _ string, offset int64) (backend.LogFollower, error) {
	b.followOffset = offset
	return b.follower, nil
}

func (b *logCaptureBackend) TaskLogPage(_ context.Context, _ string, req logfile.PageRequest) (*backend.LogPage, error) {
	b.pageReq = &req
	return &backend.LogPage{Lines: []string{}, TotalLines: -1, StartLine: -1}, nil
}

func (b *logCaptureBackend) TaskLogTail(_ context.Context, _ string, maxLines int) (*backend.LogPage, error) {
	b.tailLines = maxLines
	return &backend.LogPage{Lines: []string{}, TotalLines: -1, StartLine: -1}, nil
}

func (b *logCaptureBackend) TaskLogRead(_ context.Context, _ string, offset int64, maxLines int) (*backend.LogPage, error) {
	b.readOffset = offset
	b.readLines = maxLines
	return &backend.LogPage{Lines: []string{}, TotalLines: -1, StartLine: -1}, nil
}

func newLogTestServer(t *testing.T) (*Server, *logCaptureBackend) {
	t.Helper()
	be := &logCaptureBackend{
		UnavailableBackend: backend.NewUnavailableBackend(errors.New("unused")),
		follower:           &logStubFollower{page: &backend.LogPage{Lines: []string{"x"}, NextOffset: 77}},
	}
	return NewServer(be, &config.GlobalConfig{}), be
}

// ── SSE stream handler ───────────────────────────────────────────────────────

func TestTaskLogStream_LastEventIDBeatsOffsetParam(t *testing.T) {
	server, be := newLogTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/log/stream?offset=10&max_bytes=1024", nil)
	req.Header.Set("Last-Event-ID", "42")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if be.followOffset != 42 {
		t.Fatalf("follow offset = %d, want 42 (Last-Event-ID wins over URL offset)", be.followOffset)
	}
	if be.follower.maxBytes != 1024 {
		t.Fatalf("follower budget = %d, want 1024", be.follower.maxBytes)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "retry: 2000\n") {
		t.Fatalf("missing retry preamble in body:\n%s", body)
	}
	if !strings.Contains(body, "id: 77\nevent: lines\n") {
		t.Fatalf("missing per-event id (next_offset) in body:\n%s", body)
	}
	if !strings.Contains(body, `"next_offset":77`) {
		t.Fatalf("missing page JSON in body:\n%s", body)
	}
}

func TestTaskLogStream_OffsetParamWithoutHeader(t *testing.T) {
	server, be := newLogTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/log/stream?offset=10", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if be.followOffset != 10 {
		t.Fatalf("follow offset = %d, want 10", be.followOffset)
	}
	if be.follower.maxBytes != logfile.DefaultBudgetBytes {
		t.Fatalf("follower budget = %d, want default %d", be.follower.maxBytes, logfile.DefaultBudgetBytes)
	}
}

// ── GET handler: v2 vs legacy routing ────────────────────────────────────────

func TestTaskLog_MaxBytesBeatsLines(t *testing.T) {
	server, be := newLogTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/tasks/t1/log?max_bytes=1024&offset=99&count_lines=1&lines=50", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if be.pageReq == nil {
		t.Fatal("expected the v2 TaskLogPage path (max_bytes priority)")
	}
	want := logfile.PageRequest{Offset: 99, MaxBytes: 1024, CountLines: true}
	if *be.pageReq != want {
		t.Fatalf("page request = %+v, want %+v", *be.pageReq, want)
	}
}

func TestTaskLog_TailParamAndDefault(t *testing.T) {
	server, be := newLogTestServer(t)

	// Explicit tail=1.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/log?tail=1&max_bytes=512", nil)
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if be.pageReq == nil || !be.pageReq.Tail || be.pageReq.MaxBytes != 512 {
		t.Fatalf("tail=1 request = %+v", be.pageReq)
	}

	// v2 without offset defaults to tail (first paint has no coordinates).
	be.pageReq = nil
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/log?max_bytes=512", nil)
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if be.pageReq == nil || !be.pageReq.Tail {
		t.Fatalf("offset-less v2 request should default to tail: %+v", be.pageReq)
	}
}

func TestTaskLog_LegacyLinesPathPreserved(t *testing.T) {
	server, be := newLogTestServer(t)

	// Old CLI tail: lines only → TaskLogTail.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/log?lines=100", nil)
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if be.tailLines != 100 {
		t.Fatalf("legacy tail lines = %d, want 100", be.tailLines)
	}
	if be.pageReq != nil {
		t.Fatal("legacy request must not hit the v2 path")
	}

	// Old CLI positional read: offset+lines → TaskLogRead.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/log?offset=5&lines=100", nil)
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if be.readOffset != 5 || be.readLines != 100 {
		t.Fatalf("legacy read = (%d, %d), want (5, 100)", be.readOffset, be.readLines)
	}
}
