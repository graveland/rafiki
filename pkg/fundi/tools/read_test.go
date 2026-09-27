package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.graveland.dev/rafiki/pkg/toolmeta"

	"github.com/multigres/testkit/assert"
)

func TestReadTool(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()

	small := filepath.Join(dir, "small.txt")
	c.NoError(os.WriteFile(small, []byte("line1\nline2\nline3\n"), 0o644))

	empty := filepath.Join(dir, "empty.txt")
	c.NoError(os.WriteFile(empty, []byte(""), 0o644))

	subdir := filepath.Join(dir, "adir")
	c.NoError(os.Mkdir(subdir, 0o755))

	var bigLines []string
	for i := 1; i <= 2500; i++ {
		bigLines = append(bigLines, "L"+strconv.Itoa(i))
	}
	big := filepath.Join(dir, "big.txt")
	c.NoError(os.WriteFile(big, []byte(strings.Join(bigLines, "\n")+"\n"), 0o644))

	tests := []struct {
		name      string
		input     string
		wantErr   bool
		errSubstr string
		checkOut  func(t *testing.T, out string)
	}{
		{
			name:  "reads whole small file numbered like cat -n",
			input: fmt.Sprintf(`{"path":%q}`, small),
			checkOut: func(t *testing.T, out string) {
				want := fmt.Sprintf("%6d\t%s\n%6d\t%s\n%6d\t%s\n", 1, "line1", 2, "line2", 3, "line3")
				assert.NewAborting(t).Eq(want, out, "got:\n")
			},
		},
		{
			name:      "missing file is an error",
			input:     fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "nope.txt")),
			wantErr:   true,
			errSubstr: "no such file",
		},
		{
			name:      "relative path is rejected",
			input:     `{"path":"small.txt"}`,
			wantErr:   true,
			errSubstr: "absolute",
		},
		{
			name:      "directory path is rejected",
			input:     fmt.Sprintf(`{"path":%q}`, subdir),
			wantErr:   true,
			errSubstr: "directory",
		},
		{
			name:  "empty file reads as empty marker",
			input: fmt.Sprintf(`{"path":%q}`, empty),
			checkOut: func(t *testing.T, out string) {
				assert.NewAborting(t).StrContains(out, "empty", "expected an empty-file marker, got")
			},
		},
		{
			name:  "offset and limit page through the file",
			input: fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, small),
			checkOut: func(t *testing.T, out string) {
				// small.txt has 3 lines; offset=2,limit=1 shows only line2,
				// and since line3 remains, a paging hint is appended.
				if !strings.HasPrefix(out, fmt.Sprintf("%6d\t%s\n", 2, "line2")) {
					t.Fatalf("got:\n%q", out)
				}
				assert.NewAborting(t).StrContains(out, "offset=3", "expected a paging hint pointing at offset=3, got:\n")
			},
		},
		{
			name:  "offset and limit that reach EOF have no paging hint",
			input: fmt.Sprintf(`{"path":%q,"offset":2,"limit":2}`, small),
			checkOut: func(t *testing.T, out string) {
				want := fmt.Sprintf("%6d\t%s\n%6d\t%s\n", 2, "line2", 3, "line3")
				assert.NewAborting(t).Eq(want, out, "got:\n")
			},
		},
		{
			name:  "default cap is 2000 lines with a paging hint",
			input: fmt.Sprintf(`{"path":%q}`, big),
			checkOut: func(t *testing.T, out string) {
				c := assert.NewAborting(t)
				if !strings.Contains(out, "offset") {
					t.Fatalf("expected a paging hint mentioning offset, got tail: %q", out[len(out)-200:])
				}
				c.StrContains(out, fmt.Sprintf("%6d\t%s", 2000, "L2000"), "expected line 2000 to be the last shown line")
				c.NotStrContains(out, fmt.Sprintf("%6d\t%s", 2001, "L2001"), "did not expect line 2001 to be shown")
			},
		},
		{
			name:  "explicit limit beyond the default cap is honored",
			input: fmt.Sprintf(`{"path":%q,"limit":2200}`, big),
			checkOut: func(t *testing.T, out string) {
				assert.NewAborting(t).StrContains(out, fmt.Sprintf("%6d\t%s", 2200, "L2200"), "expected line 2200 to be shown when limit=2200")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			tr := NewFileTracker()
			rt, matErr := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
			c.NoError(matErr)
			outResult, err := rt.Execute(context.Background(), ToolInput(tc.input))
			out := outResult.Text
			if tc.wantErr {
				c.Error(err, "expected error, got output %q", out)
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			c.NoError(err, "unexpected error")
			if tc.checkOut != nil {
				tc.checkOut(t, out)
			}
		})
	}
}

func TestReadToolRecordsTrackerState(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hi\n"), 0o644))
	tr := NewFileTracker()
	rt, matErr := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(matErr)
	if _, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	c.NoError(tr.Verify(p), "expected read to satisfy the tracker, got")
}

// TestReadToolTakesPerPathLock: read shares the FileTracker with write and
// edit, and a batch runs concurrently. os.WriteFile is O_TRUNC then Write —
// not atomic — so an unlocked read can scan a file caught between the two and
// hand the model torn content, then record an mtime for that non-state.
// Asserting on the lock itself is deterministic; racing an actual torn read
// would not be.
func TestReadToolTakesPerPathLock(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello\n"), 0o644))
	tr := NewFileTracker()
	rt, matErr := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(matErr)

	unlock := tr.Lock(p)

	done := make(chan error, 1)
	go func() {
		_, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("read completed while the path was locked (err=%v); it must take the per-path lock", err)
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case err := <-done:
		c.NoError(err, "read failed once the lock was released")
	case <-time.After(5 * time.Second):
		t.Fatal("read never completed after the lock was released")
	}
}

func TestReadToolLineTruncation(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	// One 10 KB line, then two normal lines.
	longLine := strings.Repeat("x", 10*1024)
	content := longLine + "\nline2\nline3\n"
	p := filepath.Join(dir, "long.txt")
	c.NoError(os.WriteFile(p, []byte(content), 0o644))

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(err)
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.NoError(err)
	out := res.Text

	// The long line must be truncated with the suffix.
	c.StrContains(out, "… (line truncated)", "expected line truncation suffix, got:\n")
	// The suffix must appear on line 1.
	c.StrContains(out, fmt.Sprintf("%6d\t", 1), "expected line 1 to be present, got:\n")
	// Lines 2 and 3 must be intact.
	c.StrContains(out, "\n"+fmt.Sprintf("%6d\t%s\n", 2, "line2"), "expected line 2 intact, got:\n")
	c.StrContains(out, "\n"+fmt.Sprintf("%6d\t%s\n", 3, "line3"), "expected line 3 intact, got:\n")
}

func TestReadToolByteBudget(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	// Build a file large enough to exceed 50 KB.
	var lines []string
	for i := 1; i <= 800; i++ {
		lines = append(lines, fmt.Sprintf("line-%04d-%s", i, strings.Repeat("x", 100)))
	}
	p := filepath.Join(dir, "big.txt")
	c.NoError(os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(err)
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.NoError(err)
	out := res.Text

	// The whole result, trailer included, must fit under the budget. The
	// old assertion allowed a 2 KB overshoot of a 50 KB contract, and that
	// slack is exactly what hid the trailer being clipped by toolmeta.
	c.LessOrEqual(maxReadBytes+readTrailerReserve, len(out), "output too large: %d bytes (budget %d + reserve %d)", len(out), maxReadBytes, readTrailerReserve)

	// A continuation hint must be present.
	if !strings.Contains(out, "offset=") {
		t.Fatalf("expected continuation hint, got tail: %q", out[len(out)-200:])
	}

	// Extract the reported offset and verify it points to the NEXT line
	// after the last one shown.
	linesOut := strings.Split(strings.TrimRight(out, "\n"), "\n")
	lastLine := linesOut[len(linesOut)-1]
	// Parse the offset from "... offset=N ..."
	var offset int
	_, scanErr := fmt.Sscanf(lastLine, "[showing lines %d-%d; more lines remain — pass offset=%d to continue]", new(int), new(int), &offset)
	c.NoError(scanErr, "could not parse offset from %q", lastLine)
	// The offset must be > 0 and correspond to the next line.
	c.Greater(1, offset, "expected offset > 1, got")
}

func TestReadToolBinaryDetection(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "binary.bin")
	// File with a NUL byte early.
	data := []byte("hello\x00world\n")
	c.NoError(os.WriteFile(p, data, 0o644))

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(err)
	_, err = rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.Error(err, "expected an error for binary file")
	c.False(!strings.Contains(err.Error(), "binary") && !strings.Contains(err.Error(), "Binary"), "expected error to mention binary, got %v", err)
}

func TestReadToolUnderBudgetsUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	// A small file under all budgets must produce byte-identical output to
	// the existing behaviour.
	dir := t.TempDir()
	p := filepath.Join(dir, "small.txt")
	content := "line1\nline2\nline3\n"
	c.NoError(os.WriteFile(p, []byte(content), 0o644))

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(err)
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.NoError(err)
	out := res.Text
	want := fmt.Sprintf("%6d\t%s\n%6d\t%s\n%6d\t%s\n", 1, "line1", 2, "line2", 3, "line3")
	c.Eq(want, out, "got:\n")
}

func TestReadToolRoundTripContinueOffset(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	// Build a file larger than 50 KB so the first read is byte-capped.
	var lines []string
	for i := 1; i <= 1000; i++ {
		lines = append(lines, fmt.Sprintf("L%06d:%s", i, strings.Repeat("y", 80)))
	}
	p := filepath.Join(dir, "big.txt")
	c.NoError(os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	c.NoError(err)

	var seenLines []string
	offset := 0
	for {
		offset++
		input := fmt.Sprintf(`{"path":%q}`, p)
		if offset > 1 {
			input = fmt.Sprintf(`{"path":%q,"offset":%d}`, p, offset)
		}
		res, err := rt.Execute(context.Background(), ToolInput(input))
		c.NoError(err)
		out := res.Text

		// Parse emitted lines (skip the continuation trailer).
		rawLines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		nextOffset := 0
		for _, line := range rawLines {
			if strings.HasPrefix(line, "[showing lines") {
				_, _ = fmt.Sscanf(line, "[showing lines %d-%d; more lines remain — pass offset=%d to continue]", new(int), new(int), &nextOffset)
				break
			}
			// Each line is like "     1\tL000001:..."
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				seenLines = append(seenLines, strings.TrimSpace(parts[1]))
			}
		}

		if nextOffset == 0 {
			break
		}
		offset = nextOffset - 1 // the loop does offset++
	}

	// No line should appear twice.
	seen := make(map[string]bool)
	for _, l := range seenLines {
		c.False(seen[l], "duplicate line: %q", l)
		seen[l] = true
	}

	// We should have seen many lines.
	c.GreaterOrEqual(100, len(seenLines), "expected at least 100 unique lines from round-trip, got")

	// Verify no gap: the first line should be L000001.
	if !strings.HasPrefix(seenLines[0], "L000001:") {
		t.Fatalf("first line should be L000001, got %q", seenLines[0])
	}
	// The last line should be L001000 (end of file).
	if !strings.HasPrefix(seenLines[len(seenLines)-1], "L001000:") {
		t.Fatalf("last line should be L001000, got %q", seenLines[len(seenLines)-1])
	}
}

// TestReadBudgetLeavesRoomForTrailer pins the relationship between read's
// own byte budget and agentloop's blind outer cap.
//
// These are constants in two different packages with nothing but this test
// enforcing that they agree — the same duplication hazard CLAUDE.md flags
// for APP_NAME. When they were equal, read filled its budget to just under
// the cap and *then* appended the continuation trailer, pushing the result
// over; truncateToolResult cuts from the tail, so it removed the trailer
// and the last line with it. The model received a short file and no offset
// to resume from: the exact defect per-tool budgets were introduced to fix,
// invisible to every test that called Execute directly.
func TestReadBudgetLeavesRoomForTrailer(t *testing.T) {
	c := assert.NewAborting(t)
	c.Less(toolmeta.MaxToolResultSize, maxReadBytes, "maxReadBytes (%d) must be strictly below toolmeta.MaxToolResultSize (%d), "+
		"or the continuation trailer is clipped off by the outer cap", maxReadBytes, toolmeta.MaxToolResultSize)

	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 2000; i++ {
		lines = append(lines, fmt.Sprintf("line-%04d-%s", i, strings.Repeat("x", 100)))
	}
	p := filepath.Join(dir, "big.txt")
	c.NoError(os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: NewFileTracker(), Cwd: ""})
	c.NoError(err)
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.NoError(err)

	// The property that matters: the complete result, trailer included, is
	// under the outer cap, so truncateToolResult never fires on a read.
	c.LessOrEqual(toolmeta.MaxToolResultSize, len(res.Text), "read result is")
	c.StrContains(res.Text, "offset=", "byte-capped read must carry a continuation offset")
}

// TestReadTruncatesLongLineOnRuneBoundary guards against slicing a
// multi-byte rune in half. Invalid UTF-8 is silently rewritten to U+FFFD
// during JSON encoding, so the model would receive replacement characters
// rather than an honestly truncated line.
func TestReadTruncatesLongLineOnRuneBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "cjk.txt")
	// Each rune is 3 bytes, so a byte-slice at maxLineChars lands mid-rune.
	c.NoError(os.WriteFile(p, []byte(strings.Repeat("界", maxLineChars+500)+"\n"), 0o644))

	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: NewFileTracker(), Cwd: ""})
	c.NoError(err)
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.NoError(err)
	c.True(utf8.ValidString(res.Text), "read produced invalid UTF-8: a rune was split by line truncation")
	c.False(strings.ContainsRune(res.Text, utf8.RuneError), "read output contains U+FFFD: a rune was split by line truncation")
	c.StrContains(res.Text, lineTruncSuffix, "an over-long line must be marked as truncated")
}

// TestReadContinuationRoundTrip pins ReadContinuation against the trailer
// Execute actually emits: a file larger than the byte budget truncates with
// a trailer whose resume offset is lastShown+1, and a complete result parses
// as done. The pre-fill (pkg/fundi) relies on both directions.
func TestReadContinuationRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()

	var lines []string
	for i := 1; i <= 2000; i++ {
		lines = append(lines, fmt.Sprintf("line-%04d-%s", i, strings.Repeat("x", 100)))
	}
	big := filepath.Join(dir, "big.txt")
	c.NoError(os.WriteFile(big, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: NewFileTracker(), Cwd: ""})
	c.NoError(err)
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, big)))
	c.NoError(err)
	next, ok := ReadContinuation(res.Text)
	c.True(ok, "a byte-capped read must report a continuation offset")
	// lastShown is the end of the range the trailer itself names; the resume
	// offset must be exactly one past it.
	m := regexp.MustCompile(`\[showing lines (\d+)-(\d+); more lines remain`).FindStringSubmatch(res.Text)
	c.NotNil(m, "expected a showing-lines trailer in %q", res.Text[max(0, len(res.Text)-200):])
	lastShown, convErr := strconv.Atoi(m[2])
	c.NoError(convErr, "parse trailer end %q", m[2])
	c.Eq(lastShown+1, next, "ReadContinuation")

	small := filepath.Join(dir, "small.txt")
	c.NoError(os.WriteFile(small, []byte("a\nb\n"), 0o644))
	res2, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, small)))
	c.NoError(err)
	next2, ok2 := ReadContinuation(res2.Text)
	c.False(ok2, "a complete result must not report a continuation, got %d", next2)
}
