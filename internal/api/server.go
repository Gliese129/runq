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
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// DefaultPaths returns the standard daemon file paths based on ResolveDataDir().
func DefaultPaths() utils.DataDirPaths {
	_, dataDir := utils.ResolveDataDir()
	return utils.PathsFromDataDir(dataDir)
}

// DefaultSocketPath returns the resolved unix socket path.
func DefaultSocketPath() string { return DefaultPaths().SocketPath }

// DefaultPIDPath returns the resolved PID file path.
func DefaultPIDPath() string { return DefaultPaths().PIDPath }

// Deps holds all dependencies the API handlers need.
type Deps struct {
	Store     *store.Store
	Registry  *project.Registry
	Scheduler *scheduler.Scheduler
	Queue     *scheduler.Queue
	Pool      resource.Allocator
	Executor  *executor.Executor
	Logger    *slog.Logger

	// Service layer — handlers delegate business logic here.
	JobService interface {
		SubmitJob(ctx context.Context, jobCfg job.JobConfig) (string, int, error)
		ListJobs(ctx context.Context, project string) ([]service.JobSummary, error)
		ShowJob(ctx context.Context, jobID string) (*service.JobDetail, error)
		KillJob(ctx context.Context, jobID string) (int, error)
		PauseJob(ctx context.Context, jobID string) error
		ResumeJob(ctx context.Context, jobID string) error
		RemoveJob(ctx context.Context, jobID string) error
	}
	TaskService interface {
		KillTask(ctx context.Context, taskID string) error
		RetryTask(ctx context.Context, taskID string) error
	}
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
		if utils.IsProcessAlive(pid, startedTime) {
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
	_ = os.Chmod(s.socketPath, 0660)

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
	if utils.IsProcessAlive(pid, startTime) {
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

// taskRowToSchedulerTask converts a store.TaskRow to a scheduler.Task for Queue insertion.
// JSON fields (params, env) are decoded back to maps.
// taskRowToSchedulerTask delegates to service.TaskRowToSchedulerTask.
func taskRowToSchedulerTask(row *store.TaskRow) *scheduler.Task {
	return service.TaskRowToSchedulerTask(row)
}
