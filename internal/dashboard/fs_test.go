package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

// listVia hits the v1 per-target fs route and unwraps the envelope.
// The UnavailableBackend has no TargetFS, so the handler falls back to
// LocalFS — which is exactly the local-target behavior under test.
func listVia(t *testing.T, s *Server, path string) []backend.FSEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/targets/local/fs/list?path="+url.QueryEscape(path), nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fs/list %s: status %d: %s", path, rec.Code, rec.Body.String())
	}
	var out struct {
		Items []backend.FSEntry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Items
}

// Symlinked directories must list as directories (is_dir=true) so the file
// browser can enter them (the entry's own mode says "symlink"; we stat the
// target).
func TestFSListFollowsSymlinkedDirs(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir() // physically elsewhere
	t.Setenv("HOME", home)

	if err := os.WriteFile(filepath.Join(outside, "train.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "fast")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	s := NewServerWithAssets(backend.NewUnavailableBackend(nil), &config.GlobalConfig{}, "")

	// The symlink itself must appear as a directory.
	var found *backend.FSEntry
	for _, e := range listVia(t, s, home) {
		if e.Name == "fast" {
			found = &e
			break
		}
	}
	if found == nil {
		t.Fatal("symlinked dir not listed at all")
	}
	if !found.IsDir {
		t.Fatalf("symlinked dir listed with is_dir=false: %+v", found)
	}

	// Entering it must work and show the target's files.
	inside := listVia(t, s, link)
	if len(inside) != 1 || inside[0].Name != "train.py" {
		t.Fatalf("listing through symlink failed: %+v", inside)
	}
}

// Soft protection (C4): the fence is GONE by design — HPC data disks
// outside home (/scratch, /data) are a first-class use case, and the OS
// is the real permission boundary. Listing an absolute path outside home
// must simply work.
func TestFSListAllowsOutsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewServerWithAssets(backend.NewUnavailableBackend(nil), &config.GlobalConfig{}, "")
	items := listVia(t, s, outside)
	if len(items) != 1 || items[0].Name != "data.bin" {
		t.Fatalf("outside-home listing should work (soft protection): %+v", items)
	}
}

// globVia hits the v1 per-target fs/glob route (RQ2-3). Same
// LocalFS-fallback story as listVia.
func globVia(t *testing.T, s *Server, root, pattern string, extra string) (items []backend.FSEntry, truncated bool, status int) {
	t.Helper()
	q := "path=" + url.QueryEscape(root) + "&pattern=" + url.QueryEscape(pattern) + extra
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets/local/fs/glob?"+q, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, false, rec.Code
	}
	var out struct {
		Items     []backend.FSEntry `json:"items"`
		Truncated bool              `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Items, out.Truncated, rec.Code
}

func TestFSGlobResolvesPattern(t *testing.T) {
	root := t.TempDir()
	ckpts := filepath.Join(root, "checkpoints")
	if err := os.MkdirAll(ckpts, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ckpt-1000.pt", "ckpt-2000.pt", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(ckpts, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerWithAssets(backend.NewUnavailableBackend(nil), &config.GlobalConfig{}, "")

	items, truncated, _ := globVia(t, s, root, "checkpoints/ckpt-*.pt", "")
	if len(items) != 2 {
		t.Fatalf("matches = %d, want 2: %+v", len(items), items)
	}
	if items[0].Name != "ckpt-1000.pt" || items[0].Path != filepath.Join(ckpts, "ckpt-1000.pt") {
		t.Errorf("entry shape: %+v", items[0])
	}
	if items[0].Size == 0 || items[0].IsDir {
		t.Errorf("size/is_dir not filled: %+v", items[0])
	}
	if truncated {
		t.Error("truncated on a 2-match resolution")
	}
}

// Zero matches is a 200 with an empty list — "nothing there yet" is what
// the picker shows; blocking an empty sweep is the submit path's job.
func TestFSGlobNoMatchIsEmptyOK(t *testing.T) {
	s := NewServerWithAssets(backend.NewUnavailableBackend(nil), &config.GlobalConfig{}, "")
	items, _, status := globVia(t, s, t.TempDir(), "*.pt", "")
	if status != http.StatusOK || len(items) != 0 {
		t.Fatalf("status=%d items=%v", status, items)
	}
}

func TestFSGlobTruncationIsReported(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.pt", "b.pt", "c.pt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerWithAssets(backend.NewUnavailableBackend(nil), &config.GlobalConfig{}, "")
	items, truncated, _ := globVia(t, s, root, "*.pt", "&limit=2")
	if len(items) != 2 || !truncated {
		t.Fatalf("items=%d truncated=%v, want 2/true", len(items), truncated)
	}
}

func TestFSGlobRejectsBadInput(t *testing.T) {
	s := NewServerWithAssets(backend.NewUnavailableBackend(nil), &config.GlobalConfig{}, "")
	if _, _, status := globVia(t, s, t.TempDir(), "", ""); status != http.StatusBadRequest {
		t.Errorf("empty pattern: status = %d, want 400", status)
	}
	if _, _, status := globVia(t, s, t.TempDir(), "*.pt", "&limit=0"); status != http.StatusBadRequest {
		t.Errorf("limit=0: status = %d, want 400", status)
	}
	if _, _, status := globVia(t, s, t.TempDir(), "*.pt", "&limit=abc"); status != http.StatusBadRequest {
		t.Errorf("limit=abc: status = %d, want 400", status)
	}
}
