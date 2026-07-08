package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// writeTemp writes content to a temp file and returns the path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustOpen(t *testing.T, path string) *Reader {
	t.Helper()
	r, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// ── Open / Close ─────────────────────────────────────────────────────────────

func TestOpen_NonExistent(t *testing.T) {
	_, err := Open("/tmp/logfile_does_not_exist_12345.log", nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestOpen_EmptyFile(t *testing.T) {
	r := mustOpen(t, writeTemp(t, ""))
	if r.Size() != 0 {
		t.Fatalf("expected size 0, got %d", r.Size())
	}
}

func TestRefresh(t *testing.T) {
	p := writeTemp(t, "hello\n")
	r := mustOpen(t, p)
	if r.Size() != 6 {
		t.Fatalf("initial size: want 6, got %d", r.Size())
	}
	// Append data outside the Reader
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("world\n")
	f.Close()

	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	if r.Size() != 12 {
		t.Fatalf("after append: want 12, got %d", r.Size())
	}
}

// ── StripANSI ────────────────────────────────────────────────────────────────

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"empty", "", ""},
		{"sgr_color", "\x1b[32mgreen\x1b[0m text", "green text"},
		{"bold_underline", "\x1b[1;4mstyle\x1b[0m", "style"},
		{"256_color", "\x1b[38;5;196mred\x1b[0m", "red"},
		{"truecolor", "\x1b[38;2;255;0;0mred\x1b[0m", "red"},
		{"cursor_move", "\x1b[2J\x1b[H top", " top"},
		{"osc_title", "\x1b]0;my title\x07rest", "rest"},
		// \r is NOT stripped — it's real log content; frontend tqdmFolder handles folding
		{"carriage_return_preserved", "old text\rnew text", "old text\rnew text"},
		{"tqdm_cr_lf_preserved", "100%|████| 10/10 [00:01]\r\n", "100%|████| 10/10 [00:01]\r\n"},
		{"mixed", "\x1b[31mERROR\x1b[0m: something \x1b[1mfailed\x1b[0m", "ERROR: something failed"},
		{"utf8_preserved", "\x1b[32m日本語\x1b[0mテスト", "日本語テスト"},
		{"incomplete_esc_at_end", "text\x1b[", "text"},
		// \x1b + 'm' (0x6D) is NOT in C1 range [0x40,0x5F] → preserved as-is
		{"bare_esc_non_c1", "text\x1bmore", "text\x1bmore"},
		// \x1b + 'N' (0x4E, SS2) IS a valid two-byte escape → stripped
		{"bare_esc_c1", "text\x1bNmore", "textmore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripANSI(tt.in)
			if got != tt.want {
				t.Errorf("StripANSI(%q)\n  got  %q\n  want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ── ReadLines ────────────────────────────────────────────────────────────────

func TestReadLines_EmptyFile(t *testing.T) {
	r := mustOpen(t, writeTemp(t, ""))
	page, err := r.ReadLines(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 0 {
		t.Fatalf("expected 0 lines, got %d", len(page.Lines))
	}
	if page.Size != 0 {
		t.Fatalf("expected TotalBytes=0, got %d", page.Size)
	}
}

func TestReadLines_FromStart(t *testing.T) {
	content := "line0\nline1\nline2\nline3\nline4\n"
	r := mustOpen(t, writeTemp(t, content))
	page, err := r.ReadLines(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(page.Lines), page.Lines)
	}
	if page.Lines[0] != "line0" || page.Lines[1] != "line1" || page.Lines[2] != "line2" {
		t.Fatalf("unexpected lines: %v", page.Lines)
	}
	if page.Offset != 0 {
		t.Fatalf("expected StartOffset=0, got %d", page.Offset)
	}
}

func TestReadLines_SnapToLineBoundary(t *testing.T) {
	// "line0\nline1\nline2\n"
	//  01234 5 67890 1 ...
	// offset=3 is inside "line0" → backward snap to start of current line (offset 0)
	content := "line0\nline1\nline2\n"
	r := mustOpen(t, writeTemp(t, content))
	page, err := r.ReadLines(3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) < 1 {
		t.Fatal("expected at least 1 line after snap")
	}
	if page.Lines[0] != "line0" {
		t.Fatalf("expected first line 'line0' after backward snap, got %q", page.Lines[0])
	}
	if page.Offset != 0 {
		t.Fatalf("expected StartOffset=0 (start of line0), got %d", page.Offset)
	}
}

func TestReadLines_ExactLineBoundary(t *testing.T) {
	// offset=6 is exactly at start of "line1" → no snap needed
	content := "line0\nline1\nline2\n"
	r := mustOpen(t, writeTemp(t, content))
	page, err := r.ReadLines(6, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Lines[0] != "line1" {
		t.Fatalf("expected [line1], got %v", page.Lines)
	}
	if page.Offset != 6 {
		t.Fatalf("expected StartOffset=6, got %d", page.Offset)
	}
}

func TestReadLines_OffsetBeyondEOF(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "hello\n"))
	page, err := r.ReadLines(9999, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 0 {
		t.Fatalf("expected 0 lines for offset beyond EOF, got %d", len(page.Lines))
	}
}

func TestReadLines_ANSIStripped(t *testing.T) {
	content := "\x1b[31mERROR\x1b[0m: failure\n\x1b[32mOK\x1b[0m: success\n"
	r := mustOpen(t, writeTemp(t, content))
	page, err := r.ReadLines(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(page.Lines))
	}
	if page.Lines[0] != "ERROR: failure" {
		t.Fatalf("ANSI not stripped in line 0: %q", page.Lines[0])
	}
	if page.Lines[1] != "OK: success" {
		t.Fatalf("ANSI not stripped in line 1: %q", page.Lines[1])
	}
}

func TestReadLines_NoTrailingNewline(t *testing.T) {
	// Last line has no \n — should still be returned
	content := "line0\nline1"
	r := mustOpen(t, writeTemp(t, content))
	page, err := r.ReadLines(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(page.Lines), page.Lines)
	}
	if page.Lines[1] != "line1" {
		t.Fatalf("last line without newline: want 'line1', got %q", page.Lines[1])
	}
}

func TestReadLines_EndOffsetProgresses(t *testing.T) {
	content := "aaa\nbbb\nccc\n"
	r := mustOpen(t, writeTemp(t, content))

	p1, _ := r.ReadLines(0, 2)
	p2, _ := r.ReadLines(p1.NextOffset, 2)

	if p2.Offset != p1.NextOffset {
		t.Fatalf("page2 start (%d) != page1 end (%d)", p2.Offset, p1.NextOffset)
	}
	if len(p2.Lines) < 1 || p2.Lines[0] != "ccc" {
		t.Fatalf("page2: expected [ccc], got %v", p2.Lines)
	}
}

func TestReadLines_LargeLineCount(t *testing.T) {
	// Requesting more lines than MaxPageLines should be clamped
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	r := mustOpen(t, writeTemp(t, b.String()))
	page, err := r.ReadLines(0, MaxPageLines+100)
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic; may return all 100 lines (file is small)
	if len(page.Lines) > 100 {
		t.Fatalf("returned more lines than file contains: %d", len(page.Lines))
	}
}

// ── Search ───────────────────────────────────────────────────────────────────

func TestSearch_EmptyFile(t *testing.T) {
	r := mustOpen(t, writeTemp(t, ""))
	res, err := r.Search("anything", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 0 {
		t.Fatalf("expected 0 matches in empty file, got %d", len(res.Matches))
	}
	if res.Truncated {
		t.Fatal("should not be truncated")
	}
}

func TestSearch_BasicMatch(t *testing.T) {
	content := "info: all good\nerror: bad thing\ninfo: also good\nwarning: meh\n"
	r := mustOpen(t, writeTemp(t, content))
	res, err := r.Search("error", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(res.Matches))
	}
	if res.Matches[0].Line != "error: bad thing" {
		t.Fatalf("unexpected match line: %q", res.Matches[0].Line)
	}
	if res.Matches[0].LineNo != 1 {
		t.Fatalf("expected LineNo=1, got %d", res.Matches[0].LineNo)
	}
}

func TestSearch_Regex(t *testing.T) {
	content := "loss=0.234\nloss=1.567\nacc=0.95\nloss=0.012\n"
	r := mustOpen(t, writeTemp(t, content))
	res, err := r.Search(`loss=0\.\d+`, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("expected 2 regex matches, got %d: %v", len(res.Matches), res.Matches)
	}
}

func TestSearch_InvalidRegex(t *testing.T) {
	r := mustOpen(t, writeTemp(t, "hello\n"))
	_, err := r.Search("[invalid", 0, 10)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestSearch_Truncation(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("match line\n")
	}
	r := mustOpen(t, writeTemp(t, b.String()))
	res, err := r.Search("match", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 5 {
		t.Fatalf("expected 5 matches (limit), got %d", len(res.Matches))
	}
	if !res.Truncated {
		t.Fatal("expected Truncated=true")
	}
	if res.NextOffset == 0 {
		t.Fatal("expected non-zero NextOffset for pagination")
	}
}

func TestSearch_Pagination(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("hit\n")
	}
	r := mustOpen(t, writeTemp(t, b.String()))

	res1, _ := r.Search("hit", 0, 5)
	if len(res1.Matches) != 5 || !res1.Truncated {
		t.Fatalf("page1: want 5 matches truncated, got %d truncated=%v", len(res1.Matches), res1.Truncated)
	}

	res2, _ := r.Search("hit", res1.NextOffset, 100)
	if len(res2.Matches) != 5 {
		t.Fatalf("page2: want 5 matches, got %d", len(res2.Matches))
	}
	if res2.Truncated {
		t.Fatal("page2 should not be truncated")
	}
}

func TestSearch_ANSIStripped(t *testing.T) {
	// Pattern should match the stripped text, not raw ANSI
	content := "\x1b[31mERROR\x1b[0m: something failed\nall good\n"
	r := mustOpen(t, writeTemp(t, content))
	res, err := r.Search("ERROR: something", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("expected 1 match on stripped text, got %d", len(res.Matches))
	}
	// The returned line should be stripped
	if strings.Contains(res.Matches[0].Line, "\x1b") {
		t.Fatalf("match line still contains ANSI: %q", res.Matches[0].Line)
	}
}

func TestSearch_FromMiddleOffset(t *testing.T) {
	content := "aaa\nbbb\nccc\nddd\n"
	r := mustOpen(t, writeTemp(t, content))
	// Start from offset 8 → should snap to "ccc" line
	res, err := r.Search("ccc", 8, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("expected 1 match starting from mid-file, got %d", len(res.Matches))
	}
}

// ── ParseActivityTSV ─────────────────────────────────────────────────────────

func TestParseActivityTSV_ThreeColumns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "activity.tsv")
	os.WriteFile(p, []byte("1000\t4096\t102\n1060\t8192\t310\n"), 0o644)

	records, err := ParseActivityTSV(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2, got %d", len(records))
	}
	if records[0].TS != 1000 || records[0].Bytes != 4096 || records[0].Lines != 102 {
		t.Errorf("record 0: %+v", records[0])
	}
	if records[1].TS != 1060 || records[1].Bytes != 8192 || records[1].Lines != 310 {
		t.Errorf("record 1: %+v", records[1])
	}
}

func TestParseActivityTSV_TwoColumnCompat(t *testing.T) {
	// Legacy 2-column files should parse with Lines = -1
	dir := t.TempDir()
	p := filepath.Join(dir, "activity.tsv")
	os.WriteFile(p, []byte("1000\t4096\n1060\t8192\n"), 0o644)

	records, err := ParseActivityTSV(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2, got %d", len(records))
	}
	if records[0].TS != 1000 || records[0].Bytes != 4096 {
		t.Errorf("record 0: %+v", records[0])
	}
	if records[0].Lines != -1 {
		t.Errorf("2-col compat: want Lines=-1, got %d", records[0].Lines)
	}
}

func TestParseActivityTSV_NotExist(t *testing.T) {
	records, err := ParseActivityTSV("/tmp/no_such_activity_12345.tsv")
	if err != nil {
		t.Fatal("should not error for missing file")
	}
	if records != nil {
		t.Fatal("expected nil for missing file")
	}
}
