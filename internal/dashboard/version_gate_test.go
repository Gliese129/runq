package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gliese129/runq/internal/version"
)

func TestVersionGateMiddleware(t *testing.T) {
	oldV, oldMin := version.Version, version.MinClient
	defer func() { version.Version, version.MinClient = oldV, oldMin }()
	version.Version = "v0.5.0"
	version.MinClient = "v0.4.0"

	h := versionGateMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name    string
		client  string // X-Runq-Version request header; "" = browser
		status  int
		minsets string // override MinClient; "" = keep
	}{
		{name: "older than min is refused", client: "v0.3.9", status: http.StatusUpgradeRequired},
		{name: "at min passes", client: "v0.4.0", status: http.StatusOK},
		{name: "newer passes", client: "v0.9.0", status: http.StatusOK},
		{name: "browser (no header) passes", client: "", status: http.StatusOK},
		{name: "dev client passes", client: "dev", status: http.StatusOK},
		{name: "gate disabled passes old client", client: "v0.0.1", status: http.StatusOK, minsets: "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			version.MinClient = "v0.4.0"
			if c.minsets == "-" {
				version.MinClient = ""
			}
			req := httptest.NewRequest("GET", "/api/v1/health", nil)
			if c.client != "" {
				req.Header.Set("X-Runq-Version", c.client)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d", rec.Code, c.status)
			}
			// Every response, gated or not, echoes the daemon version so
			// the CLI's skew warning has something to compare against.
			if got := rec.Header().Get("X-Runq-Version"); got != "v0.5.0" {
				t.Fatalf("X-Runq-Version echo = %q, want v0.5.0", got)
			}
		})
	}
}
