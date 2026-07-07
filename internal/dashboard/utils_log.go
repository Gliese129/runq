package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/rfs"
)

// utilsLogSession tracks a single uploaded log file for the log viewer.
type utilsLogSession struct {
	path      string    // temp file path
	createdAt time.Time // for auto-cleanup
}

// utilsLogStore manages uploaded log sessions. It is embedded in Server.
type utilsLogStore struct {
	mu       sync.Mutex
	sessions map[string]*utilsLogSession
	dir      string // temp directory for uploaded logs
}

func newUtilsLogStore() *utilsLogStore {
	dir := filepath.Join(os.TempDir(), "runq-log-viewer")
	os.MkdirAll(dir, 0o700)
	s := &utilsLogStore{
		sessions: make(map[string]*utilsLogSession),
		dir:      dir,
	}
	// Background cleanup every 10 minutes
	go s.cleanupLoop()
	return s
}

const utilsLogMaxAge = 1 * time.Hour
const utilsLogMaxSize = 256 * 1024 * 1024 // 256 MB

func (s *utilsLogStore) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for id, sess := range s.sessions {
			if now.Sub(sess.createdAt) > utilsLogMaxAge {
				os.Remove(sess.path)
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *utilsLogStore) create(data io.Reader) (string, int64, error) {
	id := randomID()
	path := filepath.Join(s.dir, id+".log")

	f, err := os.Create(path)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, io.LimitReader(data, utilsLogMaxSize+1))
	f.Close()
	if err != nil {
		os.Remove(path)
		return "", 0, err
	}
	if n > utilsLogMaxSize {
		os.Remove(path)
		return "", 0, errTooLarge
	}

	s.mu.Lock()
	s.sessions[id] = &utilsLogSession{path: path, createdAt: time.Now()}
	s.mu.Unlock()
	return id, n, nil
}

func (s *utilsLogStore) get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return "", false
	}
	return sess.path, true
}

func (s *utilsLogStore) remove(id string) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		os.Remove(sess.path)
		delete(s.sessions, id)
	}
	s.mu.Unlock()
}

var errTooLarge = &httpErr{status: http.StatusRequestEntityTooLarge, msg: "log file too large (max 256 MB)"}

type httpErr struct {
	status int
	msg    string
}

func (e *httpErr) Error() string { return e.msg }

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── Handlers ────────────────────────────────────────────────────────────────

// handleUtilsLogUpload accepts a raw log body and returns a session ID.
//
//	POST /api/dashboard/utils/log
//	Body: raw log text
//	Response: { "id": "xxx", "total_bytes": N }
func (s *Server) handleUtilsLogUpload(w http.ResponseWriter, r *http.Request) {
	id, n, err := s.utilsLogs.create(r.Body)
	if err != nil {
		if he, ok := err.(*httpErr); ok {
			writeJSON(w, he.status, map[string]string{"error": he.msg})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "total_bytes": n})
}

// handleUtilsLogRead reads a page of log lines from an uploaded log.
//
//	GET /api/dashboard/utils/log/{id}
//	Query: offset, lines — same as handleTaskLog
//	Response: logfile.Page
func (s *Server) handleUtilsLogRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, ok := s.utilsLogs.get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}
	lines := logfile.DefaultPageLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= logfile.MaxPageLines {
			lines = n
		}
	}

	lr, err := logfile.Open(path, rfs.NewLocalFS())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer lr.Close()

	page, err := lr.ReadLines(offset, lines)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleUtilsLogSearch searches an uploaded log (same as job log search).
//
//	GET /api/dashboard/utils/log/{id}/search
//	Query: q, offset
//	Response: logfile.SearchResult
func (s *Server) handleUtilsLogSearch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, ok := s.utilsLogs.get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing query parameter q"})
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}

	lr, err := logfile.Open(path, rfs.NewLocalFS())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer lr.Close()

	result, err := lr.Search(q, offset, logfile.MaxSearchMatches)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleUtilsLogDelete removes an uploaded log session.
//
//	DELETE /api/dashboard/utils/log/{id}
func (s *Server) handleUtilsLogDelete(w http.ResponseWriter, r *http.Request) {
	s.utilsLogs.remove(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
