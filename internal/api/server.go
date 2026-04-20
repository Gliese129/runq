package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// DefaultSocketPath returns the default unix socket path (~/.runq/runq.sock).
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".runq", "runq.sock")
}

// DefaultPIDPath returns the default PID file path (~/.runq/daemon.pid).
func DefaultPIDPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".runq", "daemon.pid")
}

// Deps holds all dependencies the API handlers need.
type Deps struct {
	Store     *store.Store
	Registry  *project.Registry
	Scheduler *scheduler.Scheduler
	Queue     *scheduler.Queue
	Pool      *scheduler.GPUPool
	Executor  *executor.Executor
	Logger    *slog.Logger
}

// Server exposes the scheduler API over a unix domain socket.
type Server struct {
	deps       Deps
	router     *gin.Engine
	httpServer *http.Server
	listener   net.Listener
	socketPath string
	pidPath    string
	logger     *slog.Logger
	closeOnce  sync.Once
}

// NewServer creates an API server backed by Gin.
func NewServer(deps Deps, socketPath, pidPath string) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	// Recovery middleware prevents panics from crashing the daemon.
	router.Use(gin.Recovery())

	s := &Server{
		deps:       deps,
		router:     router,
		socketPath: socketPath,
		pidPath:    pidPath,
		logger:     deps.Logger,
	}

	s.registerRoutes()
	s.httpServer = &http.Server{Handler: router}

	return s
}

// Router returns the Gin engine (used by tests).
func (s *Server) Router() *gin.Engine {
	return s.router
}

// isDaemonRunning checks whether an existing daemon is alive.
// Returns (running, hasSocketFile, error).
func (s *Server) isDaemonRunning() (bool, bool, error) {
	socketPath := s.socketPath
	_, err := os.Stat(socketPath)
	if err == nil {
		// exist socket file -> check if running
		pidPath := s.pidPath
		pid, startedTime, pidErr := ReadPID(pidPath)
		if pidErr != nil {
			// no pid -> dead
			s.logger.Warn("PID file exists but can't be read, treating as stale. Consider removing: " + pidPath)
			return false, true, nil
		}
		if IsProcessAlive(pid, startedTime) {
			return true, true, nil
		}
		s.logger.Warn("Socket file exists but process is not alive, treating as stale. Consider removing: " + pidPath)
		return false, true, nil
	}
	if os.IsNotExist(err) {
		// no socket file -> not running
		return false, false, nil
	}
	return false, false, err
}

func (s *Server) Start() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}
	daemonRunning, hasSocketFile, err := s.isDaemonRunning()
	if err != nil {
		return err
	}
	// running -> skip
	if daemonRunning {
		s.logger.Info("Daemon is already running, skipping start")
		return nil
	}
	// no running + has file -> remove first
	if hasSocketFile {
		_ = os.Remove(s.socketPath)
	}

	// no running + no file -> create listener
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket: %w", err)
	}
	s.listener = l
	_ = os.Chmod(s.socketPath, 0666)

	if err := writePID(s.pidPath, time.Now()); err != nil {
		_ = s.listener.Close()
		return fmt.Errorf("failed to write pid: %w", err)
	}

	err = s.httpServer.Serve(l)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests, then removes socket and PID files.
// Safe to call multiple times (protected by sync.Once).
func (s *Server) Shutdown() error {
	var err error
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = s.httpServer.Shutdown(ctx)
		_ = os.Remove(s.socketPath)
		_ = os.Remove(s.pidPath)
	})
	return err
}

// writePID writes the current process ID to the PID file.
func writePID(path string, startTime time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create pid directory: %w", err)
	}
	data := fmt.Sprintf("%d,%s", os.Getpid(), startTime.Format(time.RFC3339Nano))
	return utils.AtomicWriteFile(path, []byte(data), 0o644)
}

// ReadPID reads the PID from a PID file. Returns 0 if file doesn't exist.
func ReadPID(path string) (int, time.Time, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	parts := strings.SplitN(string(data), ",", 2)
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

// IsProcessAlive checks if a process with the given PID exists.
func IsProcessAlive(pid int, correctStartTime time.Time) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without actually sending a signal.
	err = p.Signal(os.Signal(nil))
	if err != nil {
		return false
	}
	// if alive, check if reused by another process
	startTime, err := utils.ReadProcessStartTime(pid)
	if err != nil {
		// fallback to signal 0 result if we can't read /proc (e.g. on macOS)
		return true
	}
	// set 10 seconds tolerance
	tolerance := time.Second * 10
	diff := startTime.Sub(correctStartTime)
	if diff < -tolerance || diff > tolerance {
		return false
	}
	return true
}

// DiagnoseDaemon checks daemon health and returns a human-readable diagnostic.
// Used by CLI when connection to daemon fails, to give the user actionable guidance.
// Cleans up stale socket/PID files if the process is confirmed dead.
func DiagnoseDaemon(socketPath, pidPath string) string {
	pid, startTime, err := ReadPID(pidPath)
	if err != nil {
		return fmt.Sprintf("error reading PID file: %v", err)
	}
	if pid == 0 {
		return "runq daemon is not running.\nStart it with: runq daemon start"
	}
	if IsProcessAlive(pid, startTime) {
		return fmt.Sprintf("daemon process (PID %d) exists but is not responding.\nTry: runq daemon restart", pid)
	}
	// Process is dead — clean up stale files.
	_ = os.Remove(pidPath)
	_ = os.Remove(socketPath)
	return fmt.Sprintf("stale PID file detected (PID %d no longer running), cleaned up.\nStart with: runq daemon start", pid)
}

// Client talks to the daemon over a unix socket.
// The URL host ("http://runq") is a placeholder — DialContext routes everything
// through the socket regardless of the host value.
type Client struct {
	httpc *http.Client
}

func NewClient(socketPath string) *Client {
	return &Client{
		httpc: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

// Do sends an HTTP request to the daemon. Body is JSON-encoded if non-nil.
func (c *Client) Do(method, path string, body interface{}) (*http.Response, error) {
	url := fmt.Sprintf("http://runq%s", path)
	var bodyReader io.Reader
	if body != nil {
		buf := new(bytes.Buffer)
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
		bodyReader = buf
	}
	httpReq, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return c.httpc.Do(httpReq)
}
