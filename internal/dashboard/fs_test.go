package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gliese129/runq/internal/config"
)

// Symlinked directories must list as directories (is_dir=true) so the file
// browser can enter them; the home fence is lexical, so ~/fast -> /outside
// is traversable as long as it is reached via its home-side spelling.
func TestFSListFollowsSymlinkedDirs(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir() // physically outside "home"
	t.Setenv("HOME", home)

	if err := os.WriteFile(filepath.Join(outside, "train.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "fast")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	s := NewServerWithAssets(NewUnavailableBackend(nil), config.ModeDaemon, &config.GlobalConfig{}, "")

	list := func(path string) []FSEntry {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/dashboard/fs/list?path="+url.QueryEscape(path), nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fs/list %s: status %d: %s", path, rec.Code, rec.Body.String())
		}
		var out []FSEntry
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The symlink itself must appear as a directory.
	var found *FSEntry
	for _, e := range list(home) {
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

	// Entering it via the home-side path must work and show the target's files.
	inside := list(link)
	if len(inside) != 1 || inside[0].Name != "train.py" {
		t.Fatalf("listing through symlink failed: %+v", inside)
	}
}

// The fence still rejects paths spelled outside home.
func TestFSListRejectsLexicallyOutsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := NewServerWithAssets(NewUnavailableBackend(nil), config.ModeDaemon, &config.GlobalConfig{}, "")

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/fs/list?path=/etc", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for /etc, got %d", rec.Code)
	}
}
