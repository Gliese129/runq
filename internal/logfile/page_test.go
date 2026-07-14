package logfile

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// appendTo appends content to an existing file (simulates a running task).
func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// ── Budget packing: complete lines only, cut at last '\n' in budget ─────────

func TestReadPage_PacksCompleteLines(t *testing.T) {
	// 3 lines of 10 raw bytes each ("aaaaaaaaa\n"); budget 25 fits two
	// complete lines (20 bytes) but not the third.
	content := strings.Repeat("aaaaaaaaa\n", 3)
	r := mustOpen(t, writeTemp(t, content))

	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %v", len(p.Lines), p.Lines)
	}
	if p.NextOffset != 20 {
		t.Fatalf("NextOffset: want 20 (line boundary), got %d", p.NextOffset)
	}
	if p.Partial || p.Continues || p.Rotated {
		t.Fatalf("flags: partial=%v continues=%v rotated=%v, want all false", p.Partial, p.Continues, p.Rotated)
	}

	// Next page resumes exactly at the boundary and drains the file.
	p2, err := r.ReadPage(PageRequest{Offset: p.NextOffset, MaxBytes: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Lines) != 1 || p2.NextOffset != 30 || p2.Continues {
		t.Fatalf("page2: lines=%v next=%d continues=%v", p2.Lines, p2.NextOffset, p2.Continues)
	}
}

func TestReadPage_NeverRewinds(t *testing.T) {
	// A mid-line offset must NOT snap back to the line start (v2 contract):
	// the page starts in place with Continues=true.
	content := "abcdef\nxyz\n"
	r := mustOpen(t, writeTemp(t, content))

	p, err := r.ReadPage(PageRequest{Offset: 2, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if p.Offset != 2 {
		t.Fatalf("Offset rewound: want 2, got %d", p.Offset)
	}
	if !p.Continues {
		t.Fatal("mid-line start must set Continues")
	}
	if len(p.Lines) != 2 || p.Lines[0] != "cdef" || p.Lines[1] != "xyz" {
		t.Fatalf("lines: %v", p.Lines)
	}
	if p.NextOffset != 11 {
		t.Fatalf("NextOffset: want 11, got %d", p.NextOffset)
	}
}

// ── Fragment chain: line longer than budget, UTF-8 safe cuts ────────────────

func TestReadPage_FragmentChainUTF8(t *testing.T) {
	// One line of 20 Chinese chars (60 bytes) + '\n' = 61 bytes; budget 16.
	// 16 is not a multiple of 3, so every cut must back off to a rune
	// boundary — a byte-blind cut would split the multi-byte sequence.
	line := strings.Repeat("汉", 20)
	r := mustOpen(t, writeTemp(t, line+"\n"))

	var got strings.Builder
	offset := int64(0)
	for i := 0; i < 10; i++ { // bounded: must terminate well before 10 pages
		p, err := r.ReadPage(PageRequest{Offset: offset, MaxBytes: 16})
		if err != nil {
			t.Fatal(err)
		}
		if p.NextOffset <= offset && len(p.Lines) > 0 {
			t.Fatalf("no progress at offset %d", offset)
		}
		if offset > 0 && !p.Continues {
			t.Fatalf("chain page at offset %d must set Continues", offset)
		}
		for _, l := range p.Lines {
			if !strings.HasPrefix(l, "汉") && l != "" {
				t.Fatalf("fragment broke a rune: %q", l)
			}
			got.WriteString(l)
		}
		offset = p.NextOffset
		if !p.Partial { // final page of the chain ends at the newline
			break
		}
	}
	if got.String() != line {
		t.Fatalf("reassembled chain != original line:\n got %q\nwant %q", got.String(), line)
	}
	if offset != 61 {
		t.Fatalf("final offset: want 61, got %d", offset)
	}
}

func TestReadPage_FirstFragmentFlags(t *testing.T) {
	line := strings.Repeat("汉", 20) // 60 bytes, no newline yet, > budget
	r := mustOpen(t, writeTemp(t, line))

	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Partial || p.Continues {
		t.Fatalf("first fragment: partial=%v continues=%v, want true/false", p.Partial, p.Continues)
	}
	if p.NextOffset != 15 { // 16 backed off to the 5-rune boundary
		t.Fatalf("NextOffset: want 15 (UTF-8 boundary), got %d", p.NextOffset)
	}
	if p.Lines[0] != strings.Repeat("汉", 5) {
		t.Fatalf("fragment content: %q", p.Lines[0])
	}
}

// ── EOF: unterminated short line waits, unterminated long line fragments ────

func TestReadPage_EOFShortLineWaits(t *testing.T) {
	path := writeTemp(t, "done\npart")
	r := mustOpen(t, path)

	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 1 || p.Lines[0] != "done" {
		t.Fatalf("lines: %v (unterminated tail must be withheld)", p.Lines)
	}
	if p.NextOffset != 5 {
		t.Fatalf("NextOffset: want 5 (after last '\\n'), got %d", p.NextOffset)
	}

	// Polling at the boundary: still nothing to deliver, cursor holds.
	p2, err := r.ReadPage(PageRequest{Offset: 5, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Lines) != 0 || p2.NextOffset != 5 || p2.Partial {
		t.Fatalf("wait state: lines=%v next=%d partial=%v", p2.Lines, p2.NextOffset, p2.Partial)
	}

	// The writer finishes the line: it is delivered whole.
	appendTo(t, path, "ial\n")
	p3, err := r.ReadPage(PageRequest{Offset: 5, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Lines) != 1 || p3.Lines[0] != "partial" || p3.NextOffset != 13 {
		t.Fatalf("finished line: lines=%v next=%d", p3.Lines, p3.NextOffset)
	}
}

func TestReadPage_EOFLongLineFragments(t *testing.T) {
	// Unterminated EOF line LONGER than the budget must not wait — it is
	// delivered as a fragment chain.
	r := mustOpen(t, writeTemp(t, strings.Repeat("x", 40)))

	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Partial || len(p.Lines) != 1 || len(p.Lines[0]) != 16 {
		t.Fatalf("fragment: partial=%v lines=%v", p.Partial, p.Lines)
	}
	// Remaining 40-16=24 > 16: next window fragments again...
	p2, err := r.ReadPage(PageRequest{Offset: p.NextOffset, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Partial || !p2.Continues {
		t.Fatalf("chain page: partial=%v continues=%v", p2.Partial, p2.Continues)
	}
	// ...and the final 8 bytes (< budget, unterminated) wait for the writer.
	p3, err := r.ReadPage(PageRequest{Offset: p2.NextOffset, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Lines) != 0 || p3.NextOffset != p2.NextOffset {
		t.Fatalf("short remainder must wait: lines=%v next=%d", p3.Lines, p3.NextOffset)
	}
}

// ── Rotation ─────────────────────────────────────────────────────────────────

func TestReadPage_Rotated(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "aaa\nbbb\n"))

	p, err := r.ReadPage(PageRequest{Offset: 100, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Rotated {
		t.Fatal("size < offset must report Rotated")
	}
	if p.Offset != 0 || len(p.Lines) != 2 || p.Lines[0] != "aaa" {
		t.Fatalf("rotated first page: offset=%d lines=%v", p.Offset, p.Lines)
	}
	if p.NextOffset != 8 {
		t.Fatalf("NextOffset: want 8, got %d", p.NextOffset)
	}
}

func TestReadPage_OffsetAtSizeIsNotRotation(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "aaa\n"))
	p, err := r.ReadPage(PageRequest{Offset: 4, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if p.Rotated || len(p.Lines) != 0 || p.NextOffset != 4 {
		t.Fatalf("caught-up state: rotated=%v lines=%v next=%d", p.Rotated, p.Lines, p.NextOffset)
	}
}

func TestReadPage_RotatedToEmptyFile(t *testing.T) {
	r := mustOpen(t, writeTemp(t, ""))
	p, err := r.ReadPage(PageRequest{Offset: 50, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Rotated || len(p.Lines) != 0 || p.NextOffset != 0 {
		t.Fatalf("rotated empty: rotated=%v lines=%v next=%d", p.Rotated, p.Lines, p.NextOffset)
	}
}

// ── Tail open ────────────────────────────────────────────────────────────────

func TestReadPage_TailWholeFileFits(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "l0\nl1\n"))
	p, err := r.ReadPage(PageRequest{Tail: true, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if p.Offset != 0 || len(p.Lines) != 2 {
		t.Fatalf("tail small file: offset=%d lines=%v", p.Offset, p.Lines)
	}
}

func TestReadPage_TailAdvancesPastFirstNewline(t *testing.T) {
	// 10 lines × 6 bytes = 60. Budget 16 → window starts at 44, inside
	// "line7"; the page must start at 48 ("line8").
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("line")
		b.WriteByte(byte('0' + i))
		b.WriteByte('\n')
	}
	r := mustOpen(t, writeTemp(t, b.String()))

	p, err := r.ReadPage(PageRequest{Tail: true, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if p.Offset != 48 {
		t.Fatalf("tail offset: want 48, got %d", p.Offset)
	}
	if len(p.Lines) != 2 || p.Lines[0] != "line8" || p.Lines[1] != "line9" {
		t.Fatalf("tail lines: %v", p.Lines)
	}
	if p.Continues {
		t.Fatal("tail page starting at a line boundary must not set Continues")
	}

	// Budget 18 → window starts at 42, which IS a line start: no skip.
	p2, err := r.ReadPage(PageRequest{Tail: true, MaxBytes: 18})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Offset != 42 || len(p2.Lines) != 3 || p2.Lines[0] != "line7" {
		t.Fatalf("aligned tail: offset=%d lines=%v", p2.Offset, p2.Lines)
	}
}

func TestReadPage_TailIntoMegaLine(t *testing.T) {
	// Tail window entirely inside one huge unterminated line: start
	// mid-line and deliver fragments (Continues on the first page).
	r := mustOpen(t, writeTemp(t, "abc\n"+strings.Repeat("z", 100)))

	p, err := r.ReadPage(PageRequest{Tail: true, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if p.Offset != 104-16 {
		t.Fatalf("tail offset: want %d, got %d", 104-16, p.Offset)
	}
	if !p.Continues || !p.Partial || len(p.Lines) != 1 {
		t.Fatalf("mega tail: continues=%v partial=%v lines=%v", p.Continues, p.Partial, p.Lines)
	}
}

func TestReadPage_TailNewlineAtWindowEnd(t *testing.T) {
	// The only '\n' in the window is the file's last byte → the aligned
	// start IS the file end: empty caught-up page (literal contract).
	content := "a\n" + strings.Repeat("y", 30) + "\n" // size 33
	r := mustOpen(t, writeTemp(t, content))

	p, err := r.ReadPage(PageRequest{Tail: true, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if p.Offset != 33 || len(p.Lines) != 0 || p.NextOffset != 33 {
		t.Fatalf("boundary tail: offset=%d lines=%v next=%d", p.Offset, p.Lines, p.NextOffset)
	}
}

// ── count_lines: total_lines + start_line from one scan, cached ─────────────

func TestReadPage_CountLines(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "l0\nl1\nl2\nl3\n"))

	p, err := r.ReadPage(PageRequest{Offset: 6, MaxBytes: 1024, CountLines: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalLines != 4 {
		t.Fatalf("TotalLines: want 4, got %d", p.TotalLines)
	}
	if p.StartLine != 2 {
		t.Fatalf("StartLine: want 2, got %d", p.StartLine)
	}
	if len(p.Lines) != 2 || p.Lines[0] != "l2" {
		t.Fatalf("lines: %v", p.Lines)
	}

	// Second request hits the (path, size) cache — results must agree.
	p2, err := r.ReadPage(PageRequest{Offset: 9, MaxBytes: 1024, CountLines: true})
	if err != nil {
		t.Fatal(err)
	}
	if p2.TotalLines != 4 || p2.StartLine != 3 {
		t.Fatalf("cached count: total=%d start=%d", p2.TotalLines, p2.StartLine)
	}
}

func TestReadPage_CountLinesIsNewlineCount(t *testing.T) {
	// total_lines is the '\n' count — an unterminated tail line does not
	// increment it (frozen contract).
	r := mustOpen(t, writeTemp(t, "l0\nl1\nl2"))
	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 1024, CountLines: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalLines != 2 || p.StartLine != 0 {
		t.Fatalf("total=%d start=%d", p.TotalLines, p.StartLine)
	}
}

func TestReadPage_CountLinesOff(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "l0\nl1\n"))
	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalLines != -1 || p.StartLine != -1 {
		t.Fatalf("lazy counts: total=%d start=%d, want -1/-1", p.TotalLines, p.StartLine)
	}
}

// ── ANSI at fragment boundaries ──────────────────────────────────────────────

func TestReadPage_FragmentDoesNotSplitANSI(t *testing.T) {
	// Budget cut lands inside "\x1b[32m": the fragment must back off so
	// the escape is delivered intact on the next page.
	prefix := strings.Repeat("a", 14) // cut at 16 lands after "\x1b" (index 14)
	content := prefix + "\x1b[32m" + strings.Repeat("b", 20) + "\x1b[0m\n"
	r := mustOpen(t, writeTemp(t, content))

	p, err := r.ReadPage(PageRequest{Offset: 0, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if p.NextOffset != 14 {
		t.Fatalf("cut must back off the sliced escape: next=%d, want 14", p.NextOffset)
	}
	if p.Lines[0] != prefix {
		t.Fatalf("fragment: %q", p.Lines[0])
	}
	// Next fragment starts AT the escape and strips it cleanly.
	p2, err := r.ReadPage(PageRequest{Offset: p.NextOffset, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p2.Lines[0], "\x1b") {
		t.Fatalf("escape leaked: %q", p2.Lines[0])
	}
}

// ── Follower: rotation flag + budget chaining ────────────────────────────────

func TestFollower_RotationPage(t *testing.T) {
	path := writeTemp(t, "one\ntwo\n")
	f, err := Follow(path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p1, err := f.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Lines) != 2 || p1.Rotated {
		t.Fatalf("page1: lines=%v rotated=%v", p1.Lines, p1.Rotated)
	}

	// Rotate: rewrite the file smaller than the follower's cursor (8).
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := f.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Rotated {
		t.Fatal("follower must surface Rotated after truncation")
	}
	if p2.Offset != 0 || len(p2.Lines) != 1 || p2.Lines[0] != "new" {
		t.Fatalf("rotated page: offset=%d lines=%v", p2.Offset, p2.Lines)
	}
}

func TestFollower_FragmentChainWithBudget(t *testing.T) {
	line := strings.Repeat("汉", 20)
	path := writeTemp(t, line+"\n")
	f, err := Follow(path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.SetMaxBytes(16)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got strings.Builder
	for {
		p, err := f.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range p.Lines {
			got.WriteString(l)
		}
		if !p.Partial {
			break
		}
	}
	if got.String() != line {
		t.Fatalf("follower chain mismatch:\n got %q\nwant %q", got.String(), line)
	}
}
