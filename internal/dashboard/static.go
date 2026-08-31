package dashboard

import (
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gliese129/runq-lab/internal/config"
)

const DashboardDirEnv = "RUNQ_DASHBOARD_DIR"

var ErrDashboardAssetsUnavailable = errors.New("dashboard assets are not installed")

type StaticAssets struct {
	FS     fs.FS
	Source string
	Err    error
}

func StaticFS() fs.FS {
	return ResolveStaticAssets("").FS
}

func ResolveStaticAssets(assetsDir string) StaticAssets {
	if embedded, ok := embeddedDashboardFS(); ok {
		if err := requireDashboardIndex(embedded); err == nil {
			return StaticAssets{FS: embedded, Source: "embedded"}
		}
	}

	if strings.TrimSpace(assetsDir) != "" {
		return localStaticAssets(assetsDir, "--assets-dir")
	}

	if envDir := strings.TrimSpace(os.Getenv(DashboardDirEnv)); envDir != "" {
		return localStaticAssets(envDir, DashboardDirEnv)
	}

	return localStaticAssets(DefaultDashboardDir(), "default")
}

func DefaultDashboardDir() string {
	return filepath.Join(config.ConfigDir(), "dashboard", "dist")
}

func localStaticAssets(dir, source string) StaticAssets {
	clean := filepath.Clean(dir)
	static := os.DirFS(clean)
	if err := requireDashboardIndex(static); err != nil {
		return StaticAssets{
			Source: source,
			Err:    fmt.Errorf("%w: %s (%s)", ErrDashboardAssetsUnavailable, clean, err),
		}
	}
	return StaticAssets{FS: static, Source: clean}
}

func requireDashboardIndex(static fs.FS) error {
	if static == nil {
		return ErrDashboardAssetsUnavailable
	}
	_, err := fs.Stat(static, "index.html")
	return err
}

func writeMissingDashboard(w http.ResponseWriter, err error) {
	if err == nil {
		err = ErrDashboardAssetsUnavailable
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>runq dashboard unavailable</title>
  <style>
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f8fafc; color: #111827; }
    main { width: min(640px, calc(100vw - 48px)); border: 1px solid #d1d5db; border-radius: 8px; background: white; padding: 28px; box-shadow: 0 12px 30px rgba(15, 23, 42, 0.08); }
    h1 { margin: 0 0 12px; font-size: 22px; line-height: 1.25; }
    p { margin: 10px 0; color: #374151; line-height: 1.6; }
    code { padding: 2px 6px; border-radius: 4px; background: #eef2ff; color: #312e81; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 0.92em; }
  </style>
</head>
<body>
  <main>
    <h1>Dashboard UI is not installed</h1>
    <p>This runq binary was built without embedded dashboard assets.</p>
    <p>Use a release binary with dashboard support, or build the web UI and start the server with <code>runq dashboard --assets-dir internal/dashboard/dist</code>.</p>
    <p>Default local dashboard path: <code>%s</code></p>
    <p>Current error: <code>%s</code></p>
  </main>
</body>
</html>`, html.EscapeString(DefaultDashboardDir()), html.EscapeString(err.Error()))
}
