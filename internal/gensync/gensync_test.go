package gensync

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/genfile"
)

// waitCount polls until the counter reaches want or the deadline passes.
func waitCount(t *testing.T, c *atomic.Int32, want int32, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: events=%d want>=%d", msg, c.Load(), want)
}

func TestWatchFileSemanticEventsOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var events atomic.Int32
	var lastGen atomic.Value
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchFile(ctx, path, 20*time.Millisecond, nil, func(d *genfile.Doc) {
		events.Add(1)
		lastGen.Store(d.Generation)
	})
	time.Sleep(60 * time.Millisecond) // let it prime

	// Reformat + comment: bytes move, semantics don't — NO event.
	if err := os.WriteFile(path, []byte("# hi\na:   1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if events.Load() != 0 {
		t.Fatalf("reformat fired %d events, want 0", events.Load())
	}

	// Real change → exactly one event carrying the new generation.
	if err := os.WriteFile(path, []byte("a: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &events, 1, "semantic change not delivered")
	want, _ := genfile.SemanticHash([]byte("a: 2\n"))
	if got := lastGen.Load().(string); got != want {
		t.Errorf("event generation = %q, want %q", got, want)
	}

	// Broken yaml: no event, last good stays authoritative.
	if err := os.WriteFile(path, []byte("a: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if events.Load() != 1 {
		t.Fatalf("parse failure fired an event (events=%d)", events.Load())
	}

	// Fixed again with NEW content → one more event (recovery is a change).
	if err := os.WriteFile(path, []byte("a: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitCount(t, &events, 2, "recovery change not delivered")
}
