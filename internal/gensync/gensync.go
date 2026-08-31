// Package gensync propagates genfile changes to consumers (RQ-75) — the
// reconciler layer of config coherence, shaped after the Kubernetes
// informer: watch → deliver semantic events → subscribers converge their
// observed state onto the file's desired state.
//
// Today it hosts the single-file watcher that feeds the daemon's lane
// reconciler (config.yaml). The directory informer (OnAdd/OnChange/
// OnDelete over a workspace's .runq/config/) lands with RQ-78 and reuses
// the same byte-prefilter + semantic-compare core.
package gensync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/gliese129/runq-lab/internal/genfile"
)

// WatchFile polls path every interval and calls onChange when the SEMANTIC
// generation moves. Blocking — run it in a goroutine; returns when ctx ends.
//
// Honesty rules (mirror the reconcile philosophy):
//   - byte-hash prefilter: unchanged bytes cost one read, no parse;
//   - byte change with an UNCHANGED generation (reformat, comments) is
//     swallowed silently — not an event;
//   - a parse failure is NOT an event either: consumers keep their last
//     good state; the failure is logged (once per distinct content) and
//     the previous generation stays authoritative;
//   - a missing file is skipped: editors' save dance must not read as
//     deletion, and config.yaml vanishing is pathological — no fact was
//     learned, so no action is taken.
func WatchFile(ctx context.Context, path string, interval time.Duration, logger *slog.Logger, onChange func(*genfile.Doc)) {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	// confirmDelay bounds the torn-write window; small relative to the poll
	// interval, large relative to a write() syscall.
	confirmDelay := interval / 10
	if confirmDelay > 200*time.Millisecond {
		confirmDelay = 200 * time.Millisecond
	}
	if confirmDelay < 5*time.Millisecond {
		confirmDelay = 5 * time.Millisecond
	}

	var lastByte, lastGen, lastErrByte string
	// Prime from the current content WITHOUT firing: the consumer already
	// built its state from this file at startup — only future moves count.
	if doc, err := genfile.Load(path); err == nil && doc.ParseErr == nil {
		lastByte, lastGen = doc.ByteHash, doc.Generation
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		doc, err := genfile.Load(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // no fact learned — no action
		case err != nil:
			logger.Warn("config watch: read failed", "path", path, "error", err)
			continue
		}
		if doc.ByteHash == lastByte {
			continue // nothing moved
		}
		// Torn-write guard: os.WriteFile/scp are NOT atomic — a truncated
		// or half-written intermediate must never become an event (an empty
		// file parses to a perfectly valid nil document!). A change is only
		// believed once the bytes hold still across a confirmation re-read;
		// a file still in motion is left for the next tick.
		select {
		case <-ctx.Done():
			return
		case <-time.After(confirmDelay):
		}
		confirm, cerr := genfile.Load(path)
		if cerr != nil || confirm.ByteHash != doc.ByteHash {
			continue // in motion (or vanished) — next tick sees the settled state
		}
		if doc.ParseErr != nil {
			if doc.ByteHash != lastErrByte {
				logger.Warn("config watch: file changed but does not parse — keeping last good generation",
					"path", path, "error", doc.ParseErr)
				lastErrByte = doc.ByteHash
			}
			continue
		}
		lastErrByte = ""
		if doc.Generation == lastGen {
			lastByte = doc.ByteHash // reformat/comment only: remember, stay silent
			continue
		}
		lastByte, lastGen = doc.ByteHash, doc.Generation
		onChange(doc)
	}
}
