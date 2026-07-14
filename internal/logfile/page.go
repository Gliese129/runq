// Log stream contract v2: byte-budget page reads.
//
// ReadPage is the ONE entry point behind the dashboard's GET/SSE handlers.
// Invariants (frozen contract):
//   - A page packs only COMPLETE lines: content is cut at the last '\n'
//     inside the byte budget; NextOffset is always a line boundary and
//     NEVER rewinds.
//   - The single exception is a line longer than the budget (or an
//     unterminated EOF line longer than the budget): it is delivered as a
//     fragment chain — the page ends with Partial=true, and every page
//     whose first entry continues a previous fragment carries
//     Continues=true. Fragments are cut on UTF-8 rune boundaries.
//   - An unterminated SHORT line at EOF is withheld: the page stops at the
//     previous '\n' and waits for the writer to finish the line.
//   - size < requested offset means rotation: Rotated=true and the page
//     restarts from offset 0.
//   - Tail=true opens at size − budget advanced past the first '\n'
//     (unless that position already is a line start).
package logfile

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sync"
	"unicode/utf8"
)

// ansiPrefixRe recognises a COMPLETE CSI escape at the start of a slice —
// used to decide whether a trailing escape was sliced by a budget cut.
var ansiPrefixRe = regexp.MustCompile(`^\x1b\[[0-9;]*[a-zA-Z]`)

const (
	// DefaultBudgetBytes is the per-page byte budget when the caller does
	// not specify max_bytes.
	DefaultBudgetBytes = 256 * 1024
	// MaxBudgetBytes caps a single page (wire contract: 1MB).
	MaxBudgetBytes = 1024 * 1024
	// minBudgetBytes keeps fragment reads progressing: a fragment must fit
	// at least one full UTF-8 rune plus a sliced ANSI escape lookback.
	minBudgetBytes = 16
)

// PageRequest describes one byte-budget page read (log contract v2).
type PageRequest struct {
	// Offset is the byte position to read from. Ignored when Tail is set.
	Offset int64
	// MaxBytes is the page byte budget; <=0 means DefaultBudgetBytes,
	// values are clamped to [minBudgetBytes, MaxBudgetBytes].
	MaxBytes int64
	// Tail opens the page at size − MaxBytes, advanced past the first
	// newline (first-paint entry point without byte coordinates).
	Tail bool
	// CountLines computes TotalLines and StartLine in one sequential scan
	// (cached per (path, size) — archive files are immutable).
	CountLines bool
}

func clampBudget(n int64) int64 {
	switch {
	case n <= 0:
		return DefaultBudgetBytes
	case n < minBudgetBytes:
		return minBudgetBytes
	case n > MaxBudgetBytes:
		return MaxBudgetBytes
	}
	return n
}

// ReadPage serves one v2 page. See the package comment for the invariants.
func (r *Reader) ReadPage(req PageRequest) (*Page, error) {
	budget := clampBudget(req.MaxBytes)
	_ = r.Refresh()
	size := r.size

	offset := req.Offset
	rotated := false
	switch {
	case req.Tail:
		var err error
		offset, err = r.tailStart(size, budget)
		if err != nil {
			return nil, err
		}
	case offset < 0:
		return nil, fmt.Errorf("logfile: negative offset %d", offset)
	case offset > size:
		// Rotation: the file shrank below the caller's cursor. Restart
		// from 0 and tell the caller to reset its view. offset == size is
		// NOT rotation — it is the caught-up steady state.
		rotated = true
		offset = 0
	}

	page := &Page{
		Lines:      []string{},
		Offset:     offset,
		NextOffset: offset,
		Size:       size,
		Rotated:    rotated,
		TotalLines: -1,
		StartLine:  -1,
	}

	if offset < size {
		if offset > 0 {
			// Continues: the byte before the page is not '\n', so the first
			// entry is the continuation of an unterminated previous line
			// (fragment chain) — the client glues it onto its tail line.
			b, err := r.byteAt(offset - 1)
			if err != nil {
				return nil, err
			}
			page.Continues = b != '\n'
		}

		want := min(budget, size-offset)
		buf := make([]byte, want)
		if _, err := r.f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		// The file can shrink between Refresh and the read (live rotation
		// race): a short read is data, not an error — the next request
		// sees the new size and reports Rotated properly.
		n, err := io.ReadFull(r.f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, err
		}
		buf = buf[:n]

		if idx := bytes.LastIndexByte(buf, '\n'); idx >= 0 {
			// Normal page: complete lines only, cut at the last newline
			// inside the budget. Bytes after it (an unterminated short
			// line, or the head of a line that no longer fits) stay for
			// the next request.
			for _, raw := range bytes.Split(buf[:idx], []byte{'\n'}) {
				page.Lines = append(page.Lines, StripANSI(string(raw)))
			}
			page.NextOffset = offset + int64(idx) + 1
		} else if int64(len(buf)) == budget {
			// No newline within a FULL budget window: the current line is
			// longer than max_bytes — emit a fragment, cut on a UTF-8 rune
			// boundary (and before a sliced trailing ANSI escape).
			frag := trimIncompleteTail(buf)
			page.Lines = append(page.Lines, StripANSI(string(frag)))
			page.NextOffset = offset + int64(len(frag))
			page.Partial = true
			page.Truncated = true // legacy spelling of "last line cut mid-line"
		}
		// else: unterminated short line at EOF (want < budget, no '\n') —
		// hold position and wait for the writer; NextOffset stays put.
	}

	if req.CountLines {
		total, startLine, err := r.countLines(offset)
		if err != nil {
			return nil, err
		}
		page.TotalLines = total
		page.StartLine = startLine
	}
	return page, nil
}

// tailStart resolves Tail=true: position size − budget, advanced past the
// first newline so the page starts on a line boundary. Whole file within
// one budget → 0. Window already starting at a line boundary → no skip.
// No newline anywhere in the window → the tail is one huge line; start
// mid-line and let the fragment chain deliver it.
func (r *Reader) tailStart(size, budget int64) (int64, error) {
	if size <= budget {
		return 0, nil
	}
	pos := size - budget
	b, err := r.byteAt(pos - 1)
	if err != nil {
		return 0, err
	}
	if b == '\n' {
		return pos, nil // already a line start
	}
	if _, err := r.f.Seek(pos, io.SeekStart); err != nil {
		return 0, err
	}
	chunk := make([]byte, BufferSize)
	cur := pos
	remaining := budget // window is [pos, size), length == budget
	for remaining > 0 {
		n := min(int64(len(chunk)), remaining)
		if _, err := io.ReadFull(r.f, chunk[:n]); err != nil {
			return 0, err
		}
		if i := bytes.IndexByte(chunk[:n], '\n'); i >= 0 {
			return cur + int64(i) + 1, nil
		}
		cur += n
		remaining -= n
	}
	return pos, nil
}

// byteAt reads the single byte at pos.
func (r *Reader) byteAt(pos int64) (byte, error) {
	if _, err := r.f.Seek(pos, io.SeekStart); err != nil {
		return 0, err
	}
	var one [1]byte
	if _, err := io.ReadFull(r.f, one[:]); err != nil {
		return 0, err
	}
	return one[0], nil
}

// trimIncompleteTail backs a fragment cut off (a) a trailing ANSI escape
// that was sliced by the budget and (b) a trailing incomplete UTF-8 rune,
// so fragment boundaries never split either. It never returns an empty
// slice (progress guard: the follower/pager must always advance).
func trimIncompleteTail(raw []byte) []byte {
	out := raw
	for i := len(out) - 1; i >= 0 && i >= len(out)-64; i-- {
		if out[i] == '\x1b' {
			if i > 0 && !ansiPrefixRe.Match(out[i:]) {
				out = out[:i]
			}
			break
		}
	}
	for i := len(out) - 1; i >= 0 && i >= len(out)-utf8.UTFMax; i-- {
		if utf8.RuneStart(out[i]) {
			if i > 0 && !utf8.FullRune(out[i:]) {
				out = out[:i]
			}
			break
		}
	}
	if len(out) == 0 {
		return raw // never emit an empty fragment (would stall the chain)
	}
	return out
}

// ────────────────────────────────────────────────────────────────────────────
// Line counting (count_lines=1) — one sequential scan, cached by (path, size)
// ────────────────────────────────────────────────────────────────────────────

type lineCountKey struct {
	path string
	size int64
}

var lineCountCache = struct {
	sync.Mutex
	m map[lineCountKey]int
}{m: make(map[lineCountKey]int)}

// lineCountCacheMax bounds the cache; on overflow it is simply cleared
// (a rescan is cheap relative to unbounded growth).
const lineCountCacheMax = 128

// countLines returns the file's total newline count and the number of
// newlines before pageStart (== the 0-based absolute line number of the
// line beginning/containing pageStart), from ONE sequential scan. The
// total is cached by (path, size): a hit shortens the scan to
// [0, pageStart) — the start_line half can't be cached (it depends on the
// requested offset).
func (r *Reader) countLines(pageStart int64) (total int, startLine int, err error) {
	pageStart = min(pageStart, r.size)
	key := lineCountKey{r.path, r.size}
	lineCountCache.Lock()
	cachedTotal, hit := lineCountCache.m[key]
	lineCountCache.Unlock()

	end := r.size
	if hit {
		end = pageStart
	}
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}
	buf := make([]byte, 256*1024)
	var pos int64
	for pos < end {
		n := min(int64(len(buf)), end-pos)
		if _, err := io.ReadFull(r.f, buf[:n]); err != nil {
			return 0, 0, err
		}
		if pos+n <= pageStart {
			c := bytes.Count(buf[:n], []byte{'\n'})
			startLine += c
			total += c
		} else if pos >= pageStart {
			total += bytes.Count(buf[:n], []byte{'\n'})
		} else {
			k := pageStart - pos
			c1 := bytes.Count(buf[:k], []byte{'\n'})
			startLine += c1
			total += c1 + bytes.Count(buf[k:n], []byte{'\n'})
		}
		pos += n
	}

	if hit {
		total = cachedTotal
	} else {
		lineCountCache.Lock()
		if len(lineCountCache.m) >= lineCountCacheMax {
			lineCountCache.m = make(map[lineCountKey]int)
		}
		lineCountCache.m[key] = total
		lineCountCache.Unlock()
	}
	return total, startLine, nil
}
