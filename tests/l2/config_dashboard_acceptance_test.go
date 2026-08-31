package l2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cliFixture struct {
	t           *testing.T
	bin         string
	root        string
	dataDir     string
	runqdSocket string
	env         []string
}

func TestConfigKeysDefaultsAndRoundTrips(t *testing.T) {
	fx := newCLIFixture(t)

	// mode is dead (D9): the surviving keys are data_path + default_target.
	if got := strings.TrimSpace(fx.run("config", "get", "default_target")); got != "" {
		t.Fatalf("missing config should be unconfigured, got default %q", got)
	}

	fx.run("target", "add", "my-lab", "--gpus=0")
	fx.run("config", "set", "default_target=my-lab")
	if got := strings.TrimSpace(fx.run("config", "get", "default_target")); got != "my-lab" {
		t.Fatalf("default_target should round-trip, got %q", got)
	}

	dataPath := filepath.Join(fx.dataDir, "custom-data")
	fx.run("config", "set", "data_path="+dataPath)
	if got := strings.TrimSpace(fx.run("config", "get", "data_path")); got != dataPath {
		t.Fatalf("data_path should round-trip, got %q want %q", got, dataPath)
	}

	list := fx.run("config", "list")
	for _, want := range []string{"default_target", "my-lab", "data_path", dataPath} {
		if !strings.Contains(list, want) {
			t.Fatalf("config list should contain %q; output:\n%s", want, list)
		}
	}
}

func TestConfigSetKeepsExistingHPCSection(t *testing.T) {
	fx := newCLIFixture(t)
	configPath := fx.configPath()

	initial := strings.Join([]string{
		"data_path: " + filepath.ToSlash(filepath.Join(fx.dataDir, "data")),
		"targets:",
		"  - name: my-lab",
		"    scheduler: slurm",
		"hpc:",
		"  submit_template: sbatch --wrap '{{cmd}}'",
		"  partition: debug",
		"",
	}, "\n")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("write config with hpc section: %v", err)
	}

	fx.run("config", "set", "default_target=my-lab")
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after set: %v", err)
	}
	for _, want := range []string{"hpc:", "submit_template:", "partition: debug"} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("config set removed hpc section content %q; file:\n%s", want, string(after))
		}
	}
}

func TestDashboardConfigAPIUsesDashboardNamespace(t *testing.T) {
	fx := newCLIFixture(t)
	dataPath := filepath.Join(fx.dataDir, "configured-data")
	port := freePort(t)
	configPath := fx.configPath()
	writeConfig(t, configPath, dataPath, port)
	runqd, err := net.Listen("unix", fx.runqdSocket)
	if err != nil {
		t.Fatalf("listen on independent runqd test endpoint: %v", err)
	}
	defer runqd.Close()

	exited, output, stop := fx.startDaemon()
	defer stop()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := waitForDashboardConfig(t, baseURL, exited, output)
	if cfg.DataPath == "" {
		t.Fatal("dashboard config should include data_path")
	}
	if cfg.DataPath != dataPath {
		t.Fatalf("dashboard config data_path = %q, want %q", cfg.DataPath, dataPath)
	}
	if cfg.ConfigPath == "" {
		t.Fatal("dashboard config_path should be present")
	}
	if cfg.DefaultTarget == "" {
		t.Fatal("dashboard config should include default_target")
	}
	if len(cfg.Targets) == 0 {
		t.Fatalf("dashboard config should include targets: %+v", cfg)
	}

	errRes, err := http.Get(baseURL + "/api/v1/does-not-exist")
	if err != nil {
		t.Fatalf("request dashboard API error route: %v", err)
	}
	defer errRes.Body.Close()
	if errRes.StatusCode == http.StatusOK {
		t.Fatal("unknown dashboard API route unexpectedly returned success")
	}
	var apiErr struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(errRes.Body).Decode(&apiErr); err != nil {
		t.Fatalf("dashboard API error should be JSON: %v", err)
	}
	if apiErr.Error == "" || apiErr.Code == "" {
		t.Fatalf("dashboard API error should include error and code fields: %+v", apiErr)
	}

	res, err := http.Get(baseURL + "/api/not-dashboard/config")
	if err != nil {
		t.Fatalf("request outside dashboard API namespace: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("non-dashboard API namespace unexpectedly returned success")
	}
}

func newCLIFixture(t *testing.T) cliFixture {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "runq")
	build := exec.Command("go", "build", "-o", bin, "./cmd/runq")
	build.Dir = root
	build.Env = append(os.Environ(),
		"GOCACHE="+filepath.Join(root, ".cache", "go-build"),
		"GOMODCACHE="+filepath.Join(root, ".cache", "go-mod"),
	)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runq: %v\n%s", err, out)
	}

	shortRoot, err := os.MkdirTemp("/tmp", "runq-l2-")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })
	dataDir := filepath.Join(shortRoot, "runq-data")
	home := filepath.Join(shortRoot, "home")
	runqdSocket := filepath.Join(shortRoot, "runqd.sock")
	env := append(os.Environ(),
		"RUNQ_DATA_DIR="+dataDir,
		"RUNQD_SOCKET="+runqdSocket,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(shortRoot, "xdg-config"),
		"XDG_DATA_HOME="+filepath.Join(shortRoot, "xdg-data"),
	)
	return cliFixture{t: t, bin: bin, root: root, dataDir: dataDir, runqdSocket: runqdSocket, env: env}
}

func (fx cliFixture) run(args ...string) string {
	fx.t.Helper()
	cmd := exec.Command(fx.bin, args...)
	cmd.Dir = fx.root
	cmd.Env = fx.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		fx.t.Fatalf("runq %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (fx cliFixture) configPath() string {
	return filepath.Join(fx.dataDir, "config.yaml")
}

func (fx cliFixture) startDaemon() (chan error, *bytes.Buffer, func()) {
	fx.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, fx.bin, "daemon", "start")
	cmd.Dir = fx.root
	cmd.Env = fx.env
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		fx.t.Fatalf("start dashboard: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop := func() {
		cancel()
		<-exited
	}
	return exited, output, stop
}

type dashboardConfig struct {
	DataPath      string `json:"data_path"`
	ConfigPath    string `json:"config_path"`
	DefaultTarget string `json:"default_target"`
	Targets       []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"targets"`
}

func waitForDashboardConfig(t *testing.T, baseURL string, exited chan error, output *bytes.Buffer) dashboardConfig {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("dashboard exited before config API became available: %v\n%s", err, output.String())
		default:
		}
		res, err := http.Get(baseURL + "/api/v1/config")
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if res.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status %d", res.StatusCode)
			res.Body.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var cfg dashboardConfig
		err = json.NewDecoder(res.Body).Decode(&cfg)
		res.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if cfg.DefaultTarget == "" {
			t.Fatalf("dashboard config should include default_target: %+v", cfg)
		}
		return cfg
	}
	if lastErr != nil {
		t.Fatalf("dashboard config did not become available: %v", lastErr)
	}
	t.Fatal("dashboard config did not become available")
	return dashboardConfig{}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeConfig(t *testing.T, configPath, dataPath string, dashboardPort int) {
	t.Helper()
	contents := strings.Join([]string{
		"data_path: " + filepath.ToSlash(dataPath),
		"default_target: dashboard-test",
		"dashboard:",
		"  enabled: true",
		fmt.Sprintf("  listen: 127.0.0.1:%d", dashboardPort),
		"targets:",
		"  - name: dashboard-test",
		"",
	}, "\n")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
