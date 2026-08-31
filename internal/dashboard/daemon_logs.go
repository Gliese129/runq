package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/logfile"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/utils"
)

// ── runq self-logs (RQ-74) ──────────────────────────────────────────────────
//
// The daemon's own log is where deaths land when no UI push can carry them
// (panics before the API is up, sensor failures, forward churn). Exposing it
// read-only over the API means the user can at least read the cause without
// leaving the browser — Settings gets a "runq logs" panel on top of these:
//
//	GET /api/v1/daemon/logs                 → {files: [{name, size, mtime}]}
//	GET /api/v1/daemon/logs/{name}          → logfile page (v2: tail/offset/max_bytes/count_lines)
//	GET /api/v1/daemon/logs/{name}/stream   → SSE follow (same contract as task log streams)
//
// The name → path mapping is a fixed whitelist — this is NOT a file server.

// daemonLogFiles maps the whitelisted client log name to its on-disk path.
// runqd owns its own data/log directory and exposes diagnostics independently;
// runq-lab must not guess or serve a sibling daemon's files.
func daemonLogFiles() map[string]string {
	_, dataDir := utils.ResolveDataDir()
	return map[string]string{
		"daemon": filepath.Join(dataDir, "daemon.log"),
	}
}

type daemonLogFileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

// handleDaemonLogList — GET /daemon/logs: which self-logs exist on this
// machine. A missing file is omitted, not an error ("no runqd here" is a
// normal machine shape).
func (s *Server) handleDaemonLogList(w http.ResponseWriter, r *http.Request) {
	files := []daemonLogFileInfo{}
	for _, name := range []string{"daemon"} { // stable order
		path := daemonLogFiles()[name]
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		files = append(files, daemonLogFileInfo{Name: name, Size: info.Size(), Mtime: info.ModTime().Unix()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// resolveDaemonLog maps {name} to a whitelisted path or writes a 404.
func resolveDaemonLog(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	path, ok := daemonLogFiles()[name]
	if !ok {
		writeErr(w, http.StatusNotFound, backend.CodeNotFound, fmt.Sprintf("unknown daemon log %q (want daemon)", name))
		return "", false
	}
	return path, true
}

// handleDaemonLog — GET /daemon/logs/{name}: one byte-budget page, same v2
// contract as task logs (tail / offset / max_bytes / count_lines).
func (s *Server) handleDaemonLog(w http.ResponseWriter, r *http.Request) {
	path, ok := resolveDaemonLog(w, r)
	if !ok {
		return
	}
	rd, err := logfile.Open(path, rfs.NewLocalFS())
	if err != nil {
		writeError(w, err)
		return
	}
	defer rd.Close()

	q := r.URL.Query()
	req := logfile.PageRequest{MaxBytes: logfile.DefaultBudgetBytes}
	if v := q.Get("max_bytes"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			req.MaxBytes = n
		}
	}
	switch {
	case q.Get("tail") == "1":
		req.Tail = true
	case q.Get("offset") != "":
		n, e := strconv.ParseInt(q.Get("offset"), 10, 64)
		if e != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "invalid offset")
			return
		}
		req.Offset = n
	default:
		req.Tail = true
	}
	req.CountLines = q.Get("count_lines") == "1"

	page, err := rd.ReadPage(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleDaemonLogStream — GET /daemon/logs/{name}/stream: SSE follow,
// mirroring handleTaskLogStream (Last-Event-ID reconnect, id = next_offset).
func (s *Server) handleDaemonLogStream(w http.ResponseWriter, r *http.Request) {
	path, ok := resolveDaemonLog(w, r)
	if !ok {
		return
	}
	flusher, fok := w.(http.Flusher)
	if !fok {
		writeErr(w, http.StatusInternalServerError, backend.CodeInternal, "streaming not supported")
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}

	f, err := logfile.Follow(path, rfs.NewLocalFS(), offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	if v := r.URL.Query().Get("max_bytes"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			f.SetMaxBytes(n)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	for {
		page, err := f.Next(r.Context())
		if err != nil {
			return // ctx cancelled (client gone) or follower failed
		}
		data, _ := json.Marshal(page)
		fmt.Fprintf(w, "id: %d\nevent: lines\ndata: %s\n\n", page.NextOffset, data)
		flusher.Flush()
	}
}
