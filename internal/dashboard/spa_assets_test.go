package dashboard

import (
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Hashed build artifacts are immutable (a changed file gets a new URL);
// index.html must revalidate. Text assets gzip when the client accepts it.
func TestSPAAssetCachingAndCompression(t *testing.T) {
	body := strings.Repeat("console.log('x');\n", 64)
	s := &Server{static: fstest.MapFS{
		"index.html":         {Data: []byte("<html>app</html>")},
		"assets/app-ABC1.js": {Data: []byte(body)},
		"favicon.png":        {Data: []byte{0x89, 'P', 'N', 'G'}},
	}}

	get := func(path, acceptEnc string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if acceptEnc != "" {
			r.Header.Set("Accept-Encoding", acceptEnc)
		}
		w := httptest.NewRecorder()
		s.handleSPA(w, r)
		return w
	}

	// Hashed asset + gzip client → immutable, compressed round-trip.
	w := get("/assets/app-ABC1.js", "gzip, br")
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", cc)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if vary := w.Header().Get("Vary"); vary != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", vary)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || string(got) != body {
		t.Errorf("gzip round-trip mismatch (err %v)", err)
	}

	// No gzip in Accept-Encoding → identity body, Vary still declared.
	w = get("/assets/app-ABC1.js", "")
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("identity response has Content-Encoding %q", enc)
	}
	if w.Body.String() != body {
		t.Errorf("identity body mismatch")
	}
	if vary := w.Header().Get("Vary"); vary != "Accept-Encoding" {
		t.Errorf("identity Vary = %q, want Accept-Encoding", vary)
	}

	// index.html (SPA fallback route too) revalidates, never immutable.
	for _, path := range []string{"/", "/projects/deep/link"} {
		w = get(path, "gzip")
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, cc)
		}
	}

	// Already-compressed binary types are served as-is.
	w = get("/favicon.png", "gzip")
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("png got Content-Encoding %q", enc)
	}
}
