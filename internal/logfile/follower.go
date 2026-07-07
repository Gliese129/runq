package logfile

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/gliese129/runq/internal/rfs"
)

// Follower is a pull-based Page iterator over a growing log file. It keeps
// ONE file handle open across polls (the resource shape of a human running
// tail -f) and adapts its poll interval to activity. All content flows
// through ReadLines — the follower owns no read/clamp/strip logic.
//
// Not safe for concurrent use; one Follower per consumer.
type Follower struct {
	path     string
	fs       rfs.FS
	r        *Reader // nil until the file exists (pending task)
	offset   int64
	interval time.Duration
}

const (
	MinInterval = 300 * time.Millisecond
	MaxInterval = 2 * time.Second
)

// Follow creates a Follower starting at offset. The log file not existing
// yet is NOT an error — the task may be pending; Next waits for the file
// to appear. offset is deliberately not clamped: size < offset is the
// rotation signal Next relies on.
func Follow(path string, fsys rfs.FS, offset int64) (*Follower, error) {
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	f := &Follower{
		path:     path,
		fs:       fsys,
		offset:   offset,
		interval: MinInterval,
	}
	r, err := Open(path, fsys)
	switch {
	case err == nil:
		f.r = r
	case errors.Is(err, fs.ErrNotExist):
		// pending: lazy-open in Next
	default:
		return nil, err
	}
	return f, nil
}

// Close releases the underlying handle. Idempotent; safe when the file
// never appeared.
func (f *Follower) Close() error {
	if f.r == nil {
		return nil
	}
	err := f.r.Close()
	f.r = nil
	return err
}

// Next blocks until new data is available, then returns one page and
// advances. It returns ctx.Err() promptly on cancellation and never
// returns an empty page (it waits instead). After rotation (size <
// offset) the next page's Offset is 0 — the caller's view-reset signal.
func (f *Follower) Next(ctx context.Context) (*Page, error) {
	for {
		if f.r == nil { // pending: wait for the file to appear
			r, err := Open(f.path, f.fs)
			switch {
			case err == nil:
				f.r = r
			case errors.Is(err, fs.ErrNotExist):
				if err := f.sleep(ctx); err != nil {
					return nil, err
				}
				continue
			default:
				return nil, err
			}
		}

		if err := f.r.Refresh(); err != nil {
			// Stale/dead handle (SSH reconnect kills sftp handles for
			// good): reopen from path rather than hanging forever.
			_ = f.Close()
			if err := f.sleep(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if f.r.Size() < f.offset { // rotation: restart from 0
			f.offset = 0
		}
		if f.r.Size() > f.offset {
			page, err := f.r.ReadLines(f.offset, DefaultPageLines)
			if err != nil {
				return nil, err
			}
			f.offset = page.NextOffset
			f.interval = MinInterval // data flowing: reset backoff
			return page, nil
		}

		if err := f.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

// sleep waits one backoff interval or until ctx is done, then doubles the
// interval (capped at MaxInterval).
func (f *Follower) sleep(ctx context.Context) error {
	t := time.NewTimer(f.interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		f.interval = min(f.interval*2, MaxInterval)
		return nil
	}
}
