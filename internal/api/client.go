package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/gliese129/runq-lab/internal/version"
)

// DefaultPaths returns the standard runq-lab client-daemon paths.
func DefaultPaths() utils.DataDirPaths {
	_, dataDir := utils.ResolveDataDir()
	return utils.PathsFromDataDir(dataDir)
}

// DefaultSocketPath returns the runq-lab client daemon's Unix socket path.
func DefaultSocketPath() string { return DefaultPaths().SocketPath }

// DefaultRunqdSocketPath returns the separately installed executor's Unix
// socket path. Machine-control requests use this path, never the client socket.
func DefaultRunqdSocketPath() string {
	return utils.RunqdSocketPath()
}

// DefaultPIDPath returns the runq-lab client daemon's PID file path.
func DefaultPIDPath() string { return DefaultPaths().PIDPath }

// ReadPID reads a PID and process start time from a daemon PID file.
func ReadPID(path string) (int, time.Time, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ",", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, time.Time{}, fmt.Errorf("parse pid file: expected <pid>,<start_time>")
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("parse pid: %w", err)
	}
	startTime, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("parse start time: %w", err)
	}
	return pid, startTime, nil
}

// DiagnoseDaemon returns an actionable client-daemon diagnostic and cleans up
// stale local socket/PID files after confirming the recorded process is dead.
func DiagnoseDaemon(socketPath, pidPath string) string {
	pid, startTime, err := ReadPID(pidPath)
	if err != nil {
		return fmt.Sprintf("error reading PID file: %v", err)
	}
	if pid == 0 {
		return "runq daemon is not running.\nStart it with: runq daemon start"
	}
	if utils.IsProcessAlive(pid, startTime) {
		return fmt.Sprintf("daemon process (PID %d) exists but is not responding.\nTry: runq daemon restart", pid)
	}
	_ = os.Remove(pidPath)
	_ = os.Remove(socketPath)
	return fmt.Sprintf("stale PID file detected (PID %d no longer running), cleaned up.\nStart with: runq daemon start", pid)
}

// Client talks to an HTTP service over a Unix socket.
type Client struct {
	httpc *http.Client
}

const (
	DefaultClientTimeout = 10 * time.Second
	SubmitClientTimeout  = 50 * time.Second
)

func NewClient(socketPath string) *Client {
	return NewClientWithTimeout(socketPath, DefaultClientTimeout)
}

func NewClientWithTimeout(socketPath string, timeout time.Duration) *Client {
	dialer := &net.Dialer{}
	return &Client{
		httpc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Do sends an HTTP request. Body is JSON-encoded when non-nil.
func (c *Client) Do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	return c.do(ctx, c.httpc, method, path, body)
}

func (c *Client) DoWithTimeout(ctx context.Context, method, path string, body interface{}, timeout time.Duration) (*http.Response, error) {
	httpc := *c.httpc
	httpc.Timeout = timeout
	return c.do(ctx, &httpc, method, path, body)
}

func (c *Client) do(ctx context.Context, httpc *http.Client, method, path string, body interface{}) (*http.Response, error) {
	url := fmt.Sprintf("http://runq%s", path)
	var bodyReader io.Reader
	if body != nil {
		buf := new(bytes.Buffer)
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
		bodyReader = buf
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Runq-Version", version.Version)
	// Harmless on the runq-lab dashboard API, required when this transport is
	// pointed at the independently versioned runqd machine protocol.
	req.Header.Set("X-Runqd-Protocol", MachineProtocolVersion)
	resp, err := httpc.Do(req)
	if err != nil {
		if os.Getenv("RUNQ_SOCKET") != "" {
			return nil, fmt.Errorf("%w\n(RUNQ_SOCKET is set — if this socket is forwarded from another machine, the runq daemon there may be offline or the tunnel down)", err)
		}
		return nil, err
	}
	warnVersionSkew(resp.Header.Get("X-Runq-Version"))
	return resp, nil
}

var warnSkewOnce sync.Once

func warnVersionSkew(daemonVersion string) {
	if daemonVersion == "" || daemonVersion == version.Version {
		return
	}
	if _, ok := version.Compare(daemonVersion, version.Version); !ok {
		return
	}
	warnSkewOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "warning: runq %s talking to daemon %s — rerun `runq connect` (remote) or reinstall to match\n",
			version.Version, daemonVersion)
	})
}
