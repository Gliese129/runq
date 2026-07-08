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
