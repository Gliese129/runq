package dashboard

// Remote-CLI forward guard (RQ-45). The forwarded socket on a login node
// turns "compromised cluster account" into "full proxy of the laptop
// daemon" unless the surface is scoped. The guard enforces the remote
// CLI's honest worldview — "I am runq on THIS cluster, and only this
// cluster":
//
//   - identity is the TRANSPORT, not a credential: each target's forward
//     wraps its own guard instance, so the target name is bound at wiring
//     time. A token would be readable by exactly the uid that can already
//     open the socket — a credential to manage, no boundary gained.
//   - default-deny route allowlist; config/target mutation, log-sessions,
//     and everything belonging to OTHER targets is refused with 403.
//   - clean IS allowed, forced to this target: an attacker with the ssh
//     account can already rm -rf this cluster's files — blocking clean
//     here protects nothing (the cross-target reach is what must die).
//   - submit/plan/preview bodies get target forced-or-verified; job/task
//     actions resolve ownership through the store first (killing another
//     cluster's job from here is exactly the escalation this exists for).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gliese129/runq-lab/internal/backend"
)

// guardBodyLimit caps buffered request bodies (submit configs are small).
const guardBodyLimit = 10 << 20

// targetOwner is the store-backed ownership lookup (MultiBackend).
type targetOwner interface {
	JobTarget(ctx context.Context, jobID string) (string, error)
	TaskTarget(ctx context.Context, taskID string) (string, error)
}

// RemoteCLIHandler wraps the full v1 mux in the forward guard for one
// target. Everything not explicitly allowed is 403.
func (s *Server) RemoteCLIHandler(target string) http.Handler {
	owner, _ := s.backend.(targetOwner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.guardAllows(w, r, target, owner) {
			s.Handler().ServeHTTP(w, r)
		}
	})
}

// guardAllows applies the allowlist. It may REWRITE r (query/body target
// scoping) before approving. On refusal it writes the 403 itself.
func (s *Server) guardAllows(w http.ResponseWriter, r *http.Request, target string, owner targetOwner) bool {
	deny := func(what string) bool {
		writeErr(w, http.StatusForbidden, backend.CodeForbidden,
			fmt.Sprintf("%s is not available through the remote-CLI forward (scoped to target %q)", what, target))
		return false
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if path == r.URL.Path { // not under /api/v1 (static SPA etc.)
		return deny("non-API path")
	}
	seg := strings.Split(path, "/")
	get := r.Method == http.MethodGet

	switch seg[0] {
	case "health", "config":
		return get || deny(r.Method+" "+path)

	case "projects":
		// Read-only: a poisoned command_template would execute wherever a
		// LAPTOP user later submits that project — cross-target via human.
		return get || deny("project mutation")

	case "jobs":
		switch {
		case len(seg) == 1 && get: // GET /jobs — scope the listing
			q := r.URL.Query()
			q.Set("target", target)
			r.URL.RawQuery = q.Encode()
			return true
		case len(seg) == 1 && r.Method == http.MethodPost: // submit
			return s.rewriteBodyTarget(w, r, target)
		case len(seg) == 2 && (seg[1] == "plan" || seg[1] == "preview") && r.Method == http.MethodPost:
			return s.rewriteBodyTarget(w, r, target)
		case len(seg) >= 2: // /jobs/{id}[/...] — ownership gate
			return s.checkOwner(w, r, target, "job", seg[1], owner)
		}
		return deny(r.Method + " " + path)

	case "tasks":
		switch {
		case len(seg) == 1 && get:
			q := r.URL.Query()
			q.Set("target", target)
			r.URL.RawQuery = q.Encode()
			return true
		case len(seg) >= 2:
			return s.checkOwner(w, r, target, "task", seg[1], owner)
		}
		return deny(r.Method + " " + path)

	case "targets":
		// Only THIS target's resources, and only the non-mutating ones
		// (+refresh, +parse-script). PUT/DELETE/check/connect stay refused.
		if len(seg) < 3 || seg[1] != target {
			return deny(r.Method + " " + path)
		}
		rest := strings.Join(seg[2:], "/")
		switch {
		case get && (rest == "gpus" || rest == "fs/list" || rest == "fs/read" || rest == "python-envs"):
			return true
		case r.Method == http.MethodPost && (rest == "refresh" || rest == "fs/parse-script"):
			return true
		}
		return deny(r.Method + " " + path)

	case "clean":
		if r.Method != http.MethodPost {
			return deny(r.Method + " " + path)
		}
		// Same-target clean is allowed: the ssh account can already delete
		// this cluster's files by hand. Force the scope so shared-DB
		// selectors (job_id, older_than) cannot reach other targets.
		return s.forceBodyField(w, r, "target", target)
	}

	return deny(r.Method + " " + path)
}

// checkOwner resolves which target owns the addressed job/task and admits
// only this forward's own. Fail closed: no resolver, lookup error, or
// unknown id (let the real handler 404 AFTER we know it's not a foreign id
// — an unknown id resolves to "", which never equals a target name).
func (s *Server) checkOwner(w http.ResponseWriter, r *http.Request, target, kind, id string, owner targetOwner) bool {
	if owner == nil {
		writeErr(w, http.StatusForbidden, backend.CodeForbidden, "ownership resolution unavailable — refusing by default")
		return false
	}
	var got string
	var err error
	if kind == "job" {
		got, err = owner.JobTarget(r.Context(), id)
	} else {
		got, err = owner.TaskTarget(r.Context(), id)
	}
	if err != nil {
		writeError(w, err)
		return false
	}
	if got == "" {
		// Unknown id: pass through for an honest 404 from the real handler
		// (a guard 403 here would leak "this id exists elsewhere").
		return true
	}
	if got != target {
		writeErr(w, http.StatusForbidden, backend.CodeForbidden,
			fmt.Sprintf("%s %q belongs to another target", kind, id))
		return false
	}
	return true
}

// rewriteBodyTarget enforces the submit-family contract: body.target empty
// → forced to this target; set to anything else → refused.
func (s *Server) rewriteBodyTarget(w http.ResponseWriter, r *http.Request, target string) bool {
	doc, ok := s.readBodyDoc(w, r)
	if !ok {
		return false
	}
	if t, _ := doc["target"].(string); t != "" && t != target {
		writeErr(w, http.StatusForbidden, backend.CodeForbidden,
			fmt.Sprintf("cannot submit to target %q through target %q's forward", t, target))
		return false
	}
	doc["target"] = target
	return s.replaceBody(w, r, doc)
}

// forceBodyField overwrites one body field unconditionally.
func (s *Server) forceBodyField(w http.ResponseWriter, r *http.Request, field, value string) bool {
	doc, ok := s.readBodyDoc(w, r)
	if !ok {
		return false
	}
	doc[field] = value
	return s.replaceBody(w, r, doc)
}

func (s *Server) readBodyDoc(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, guardBodyLimit))
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "read body: "+err.Error())
		return nil, false
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "invalid JSON body: "+err.Error())
			return nil, false
		}
	}
	return doc, true
}

func (s *Server) replaceBody(w http.ResponseWriter, r *http.Request, doc map[string]any) bool {
	buf, err := json.Marshal(doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, backend.CodeInternal, "re-encode body: "+err.Error())
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	return true
}
