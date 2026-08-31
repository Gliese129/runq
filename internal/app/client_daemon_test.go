package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/utils"
)

func TestNewClientDaemonWithoutTargetsDoesNotRequireRunqd(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dataDir)
	t.Setenv("RUNQD_SOCKET", filepath.Join(dataDir, "missing-runqd.sock"))

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	paths := utils.PathsFromDataDir(dataDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d, err := newClientDaemon(dataDir, paths, logger, st)
	if err != nil {
		t.Fatalf("newClientDaemon() required runqd in unconfigured state: %v", err)
	}
	if len(d.lanes) != 0 {
		t.Fatalf("boot lanes = %+v, want none", d.lanes)
	}
	if got := d.multiBe.DefaultTargetName(); got != "" {
		t.Fatalf("default target = %q, want empty", got)
	}
	if _, err := d.multiBe.ListJobs(context.Background(), ""); err != nil {
		t.Fatalf("status/list path must remain available: %v", err)
	}
	if _, err := d.multiBe.DryRun(context.Background(), job.JobConfig{}); !errors.Is(err, backend.ErrNoTargetConfigured) {
		t.Fatalf("target-dependent path error = %v, want ErrNoTargetConfigured", err)
	}
}

func TestConnectRunqdEndpointAcceptsConfiguredSocket(t *testing.T) {
	tmp, err := os.CreateTemp("/tmp", "runqd-endpoint-*.sock")
	if err != nil {
		t.Fatalf("reserve short socket path: %v", err)
	}
	socket := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(socket)
	t.Cleanup(func() { _ = os.Remove(socket) })

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on test socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if err := connectRunqdEndpoint(socket); err != nil {
		t.Fatalf("connectRunqdEndpoint() error = %v", err)
	}
}

func TestConnectRunqdEndpointReturnsActionableError(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing-runqd.sock")
	err := connectRunqdEndpoint(socket)
	if err == nil {
		t.Fatal("connectRunqdEndpoint() error = nil, want connection failure")
	}

	message := err.Error()
	for _, want := range []string{socket, "start runqd independently", "RUNQD_SOCKET"} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not contain %q", message, want)
		}
	}
}

func TestSynthLocalRunqdTargetDoesNotLocateOrRewriteAdapter(t *testing.T) {
	target, err := synthLocalRunqdTarget(config.TargetConfig{Name: "local"}, t.TempDir())
	if err != nil {
		t.Fatalf("synthLocalRunqdTarget() error = %v", err)
	}

	for name, template := range map[string]string{
		"submit":      target.SubmitTemplate,
		"status":      target.StatusTemplate,
		"status_list": target.StatusListTemplate,
		"kill":        target.KillTemplate,
		"gpu":         target.GPUTemplate,
	} {
		if !strings.HasPrefix(template, "runqd ") {
			t.Errorf("%s template = %q, want unchanged runqd adapter command", name, template)
		}
	}
}
