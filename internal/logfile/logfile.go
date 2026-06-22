// Package logfile provides byte-offset aware log reading with ANSI stripping
// and streaming search. It replaces the whole-file-in-memory approach used by
// the existing handleTaskLog handlers.
//
// Design invariants:
//   - Never load the entire file into memory (GB-scale logs are common).
//   - Seek to a byte offset, snap backward to the current line's start, then
//     stream N lines via bufio.Reader.
//   - ANSI escape codes are stripped in the backend so the frontend and search
//     patterns operate on clean text.
//   - All offsets are byte offsets into the raw (pre-strip) file, so callers
//     can Seek back to the same position regardless of stripping.
package logfile

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
)

// DefaultPageLines is the number of lines returned when the caller doesn't
// specify a count.
const DefaultPageLines = 200

// MaxPageLines caps a single read to prevent abuse.
const MaxPageLines = 5000

// MaxSearchMatches caps the number of matches returned per search request.
const MaxSearchMatches = 500

const BufferSize = 64 * 1024 // 64KB

// ────────────────────────────────────────────────────────────────────────────
// Reader
// ────────────────────────────────────────────────────────────────────────────

// Reader wraps a log file with byte-offset aware reading and ANSI stripping.
// It is NOT safe for concurrent use; callers must synchronise externally or
// open one Reader per goroutine.
type Reader struct {
	f    *os.File
	path string
	size int64 // cached at Open; refreshed by Refresh()
}

// Open opens a log file for reading. The caller must call Close when done.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("logfile.Open: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("logfile.Open stat: %w", err)
	}
	return &Reader{f: f, path: path, size: info.Size()}, nil
}

// Close releases the underlying file handle.
func (r *Reader) Close() error { return r.f.Close() }

// Size returns the file size as of the last Open or Refresh.
func (r *Reader) Size() int64 { return r.size }

// Refresh re-stats the file to pick up new writes (running task).
func (r *Reader) Refresh() error {
	info, err := r.f.Stat()
	if err != nil {
		return err
	}
	r.size = info.Size()
	return nil
}

// ────────────────────────────────────────────────────────────────────────────
// Page — the return type for ReadLines
// ────────────────────────────────────────────────────────────────────────────

// Page is a window of log lines anchored by byte offsets.
type Page struct {
	// Lines contains the ANSI-stripped text of each line (no trailing \n).
	Lines []string `json:"lines"`

	// StartOffset is the byte offset in the raw file where the first
	// returned line begins (after snap-to-line-boundary).
	StartOffset int64 `json:"start_offset"`

	// EndOffset is the byte offset immediately after the last byte of the
	// last returned line (i.e. pointing at the \n or EOF).
	EndOffset int64 `json:"end_offset"`

	// TotalBytes is the file size as of this read.
	TotalBytes int64 `json:"total_bytes"`

	// TotalLines is the total line count. Computing this requires a full
	// scan, so it is set to -1 when unknown (large files). Callers must
	// handle -1 gracefully.
	TotalLines int `json:"total_lines"`
}

// ReadLines reads up to n lines starting from the given byte offset.
//
// Algorithm:
//  1. Seek to offset; snap backward to current line's start.
//  2. Read up to n lines via bufio.Reader.ReadBytes('\n').
//  3. Strip ANSI; track raw byte positions for StartOffset / EndOffset.
//  4. TotalBytes from r.size; TotalLines = -1 (lazy).
//
// Returns an empty Page (not an error) when offset >= file size.
func (r *Reader) ReadLines(offset int64, n int) (*Page, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	startOffset, err := snapToLineStart(f, offset, r.size)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(f, BufferSize)
	currOffset := startOffset
	lines := make([]string, 0, n)
	for len(lines) < n {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			line := trimNewline(raw)
			lines = append(lines, StripANSI(string(line)))
			currOffset += int64(len(raw))
		}
		if err != nil { // EOF or other error
			break
		}
	}

	return &Page{
		Lines:       lines,
		StartOffset: startOffset,
		EndOffset:   currOffset,
		TotalBytes:  r.size,
		TotalLines:  -1, // lazy: full-file line count not computed on read path
	}, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Search
// ────────────────────────────────────────────────────────────────────────────

// Match is a single search hit.
type Match struct {
	// LineNo is the 0-based line number within the file.
	LineNo int `json:"line_no"`

	// Offset is the byte offset of the start of this line in the raw file.
	Offset int64 `json:"offset"`

	// Line is the ANSI-stripped matched line.
	Line string `json:"line"`
}

// SearchResult holds a batch of search matches plus pagination state.
type SearchResult struct {
	// Matches contains up to `limit` hits.
	Matches []Match `json:"matches"`

	// NextOffset is the byte offset to pass as startOffset in the next call
	// to continue pagination. 0 when the entire file has been scanned.
	NextOffset int64 `json:"next_offset"`

	// Truncated is true when more matches exist beyond `limit`.
	Truncated bool `json:"truncated"`
}

// Search scans the file from startOffset, matching each ANSI-stripped line
// against the compiled pattern, returning up to limit matches.
//
// Algorithm:
//  1. Seek + snap backward to line boundary.
//  2. Stream lines via bufio.Reader.ReadBytes('\n'); strip ANSI; test regexp.
//  3. On match, record LineNo (0-based from file start when startOffset==0,
//     else -1), Offset (raw byte position of line start), Line.
//  4. After `limit` matches, set Truncated=true and NextOffset to the byte
//     after the last scanned line.
//  5. If EOF is reached before `limit`, Truncated=false, NextOffset=0.
//
// The pattern string is compiled with regexp.Compile (not MustCompile);
// invalid patterns return an error.
func (r *Reader) Search(pattern string, startOffset int64, limit int) (*SearchResult, error) {
	if r.f == nil {
		return nil, fmt.Errorf("file is nil")
	}
	startOffset, err := snapToLineStart(r.f, startOffset, r.size)
	if err != nil {
		return nil, err
	}
	if _, err := r.f.Seek(startOffset, io.SeekStart); err != nil {
		return nil, err
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	matches := make([]Match, 0)
	truncated := false

	lineStart := startOffset
	lineCnt := 0
	br := bufio.NewReaderSize(r.f, BufferSize)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			lineCnt++
			lineNo := -1
			if startOffset == 0 {
				lineNo = lineCnt - 1
			}
			text := StripANSI(string(trimNewline(raw)))
			if re.MatchString(text) {
				matches = append(matches, Match{
					Line:   text,
					Offset: lineStart,
					LineNo: lineNo,
				})
			}
			lineStart += int64(len(raw))
		}
		if err != nil || len(matches) >= limit {
			if len(matches) >= limit && err == nil {
				truncated = true
			}
			break
		}
	}

	nextOffset := int64(0) // 0 = entire file scanned
	if truncated {
		nextOffset = lineStart
	}
	return &SearchResult{
		Matches:    matches,
		NextOffset: nextOffset,
		Truncated:  truncated,
	}, nil
}

type ansiStatus int

const (
	statusNormal ansiStatus = iota
	statusEscStart
	statusCsi
	statusCsiIntermediate
	statusOsc
	statusOscEsc
)

// ────────────────────────────────────────────────────────────────────────────
// ANSI stripping (exported for testing / reuse)
// ────────────────────────────────────────────────────────────────────────────

// StripANSI removes ANSI escape sequences from s, returning clean text.
//
// This is the single chokepoint: both ReadLines and Search call it on every
// line before returning or matching. Performance matters — see the design doc
// for a state-machine approach vs regexp approach.
//
// Must handle: CSI sequences (\x1b[...m and friends), OSC (\x1b]...\x07),
// simple escapes (\x1b[A, \x1b[2J). Does NOT handle \r — that is real log
// content (tqdm overwrites), left for the frontend tqdmFolder processor.
func StripANSI(s string) string {
	buf := make([]byte, 0, len(s))
	var escStartIndex int

	status := statusNormal

	for i := 0; i < len(s); i++ {
		b := s[i]
		switch status {
		case statusNormal:
			if b == '\x1b' {
				status = statusEscStart
				escStartIndex = i
				continue
			}
			buf = append(buf, b)
		case statusEscStart:
			if b == '[' {
				status = statusCsi
				continue
			}
			if b == ']' {
				status = statusOsc
				continue
			}
			if b >= 0x40 && b <= 0x5F {
				status = statusNormal
				continue
			}
			// corner case: write \x1b back
			status = statusNormal
			buf = append(buf, s[escStartIndex:i+1]...)
		case statusCsi:
			if b >= 0x30 && b <= 0x3F {
				// CSI Params
				continue
			}
			if b >= 0x20 && b <= 0x2F {
				status = statusCsiIntermediate
				continue
			}
			if b >= 0x40 && b <= 0x7E {
				status = statusNormal
				continue
			}
			// illegal string -> add chars back
			status = statusNormal
			buf = append(buf, s[escStartIndex:i+1]...)
		case statusCsiIntermediate:
			if b >= 0x40 && b <= 0x7E {
				status = statusNormal
				continue
			}
			// illegal string -> add chars back
			status = statusNormal
			buf = append(buf, s[escStartIndex:i+1]...)
		case statusOsc:
			if b == '\x07' {
				// BEL
				status = statusNormal
				continue
			}
			if b == '\x1b' {
				// maybe \x1b\\ (ST)
				status = statusOscEsc
			}
		default: // =statusOscEsc
			if b == '\\' {
				status = statusNormal
				continue
			}
			status = statusOsc
		}
	}
	// Trailing incomplete escape sequences are silently dropped (broken ANSI).
	// Only statusNormal content has already been written to buf.
	return string(buf)
}

// ────────────────────────────────────────────────────────────────────────────
// Activity TSV (3-column: ts, bytes, lines — written incrementally by sidecar)
// ────────────────────────────────────────────────────────────────────────────

// ActivityRecord is one row from activity.tsv: {timestamp, cumulative_bytes, cumulative_lines}.
// The sidecar writes all three columns incrementally every 60s.
// For backward compatibility with old 2-column files, Lines defaults to -1.
type ActivityRecord struct {
	TS    int64 `json:"ts"`
	Bytes int64 `json:"bytes"`
	Lines int64 `json:"lines"`
}

// ParseActivityTSV parses an activity.tsv file into records.
// Supports both 3-column (ts, bytes, lines) and legacy 2-column (ts, bytes) format.
// Legacy 2-column rows get Lines = -1.
func ParseActivityTSV(path string) ([]ActivityRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("parse activity.tsv: %w", err)
	}
	var records []ActivityRecord
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		fields := bytes.Split(line, []byte("\t"))
		if len(fields) < 2 {
			continue
		}
		rec := ActivityRecord{Lines: -1} // default for 2-col compat
		fmt.Sscanf(string(fields[0]), "%d", &rec.TS)
		fmt.Sscanf(string(fields[1]), "%d", &rec.Bytes)
		if len(fields) >= 3 {
			fmt.Sscanf(string(fields[2]), "%d", &rec.Lines)
		}
		records = append(records, rec)
	}
	return records, nil
}

func snapToLineStart(f *os.File, offset int64, fileSize int64) (lineStart int64, err error) {
	if offset == 0 {
		return 0, nil
	}
	if offset >= fileSize {
		return fileSize, nil
	}
	const chunkSize = int64(512)
	buf := make([]byte, chunkSize)
	currOffset := offset
	for currOffset > 0 {
		readSize := min(currOffset, chunkSize)
		currOffset -= readSize
		if _, err := f.Seek(currOffset, io.SeekStart); err != nil {
			return 0, err
		}
		n, err := f.Read(buf[:readSize])
		if err != nil && err != io.EOF {
			return 0, err
		}
		idx := bytes.LastIndexByte(buf[:n], '\n')
		if idx != -1 {
			return currOffset + int64(idx) + 1, nil // +1: past the \n to actual line start
		}
	}
	return 0, nil
}

func trimNewline(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		return raw[:len(raw)-1]
	}
	return raw
}
