package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadTool(t *testing.T) {
	dir := t.TempDir()

	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	var bigLines []string
	for i := 1; i <= 2500; i++ {
		bigLines = append(bigLines, "L"+strconv.Itoa(i))
	}
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Join(bigLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
				if out != want {
					t.Fatalf("got:\n%q\nwant:\n%q", out, want)
				}
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
				if !strings.Contains(out, "empty") {
					t.Fatalf("expected an empty-file marker, got %q", out)
				}
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
				if !strings.Contains(out, "offset=3") {
					t.Fatalf("expected a paging hint pointing at offset=3, got:\n%q", out)
				}
			},
		},
		{
			name:  "offset and limit that reach EOF have no paging hint",
			input: fmt.Sprintf(`{"path":%q,"offset":2,"limit":2}`, small),
			checkOut: func(t *testing.T, out string) {
				want := fmt.Sprintf("%6d\t%s\n%6d\t%s\n", 2, "line2", 3, "line3")
				if out != want {
					t.Fatalf("got:\n%q\nwant:\n%q", out, want)
				}
			},
		},
		{
			name:  "default cap is 2000 lines with a paging hint",
			input: fmt.Sprintf(`{"path":%q}`, big),
			checkOut: func(t *testing.T, out string) {
				if !strings.Contains(out, "offset") {
					t.Fatalf("expected a paging hint mentioning offset, got tail: %q", out[len(out)-200:])
				}
				if !strings.Contains(out, fmt.Sprintf("%6d\t%s", 2000, "L2000")) {
					t.Fatalf("expected line 2000 to be the last shown line")
				}
				if strings.Contains(out, fmt.Sprintf("%6d\t%s", 2001, "L2001")) {
					t.Fatalf("did not expect line 2001 to be shown")
				}
			},
		},
		{
			name:  "explicit limit beyond the default cap is honored",
			input: fmt.Sprintf(`{"path":%q,"limit":2200}`, big),
			checkOut: func(t *testing.T, out string) {
				if !strings.Contains(out, fmt.Sprintf("%6d\t%s", 2200, "L2200")) {
					t.Fatalf("expected line 2200 to be shown when limit=2200")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewFileTracker()
			rt, matErr := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
			if matErr != nil {
				t.Fatal(matErr)
			}
			outResult, err := rt.Execute(context.Background(), ToolInput(tc.input))
			out := outResult.Text
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got output %q", out)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.checkOut != nil {
				tc.checkOut(t, out)
			}
		})
	}
}

func TestReadToolRecordsTrackerState(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	rt, matErr := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if matErr != nil {
		t.Fatal(matErr)
	}
	if _, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p))); err != nil {
		t.Fatal(err)
	}
	if err := tr.Verify(p); err != nil {
		t.Fatalf("expected read to satisfy the tracker, got %v", err)
	}
}

// TestReadToolTakesPerPathLock: read shares the FileTracker with write and
// edit, and a batch runs concurrently. os.WriteFile is O_TRUNC then Write —
// not atomic — so an unlocked read can scan a file caught between the two and
// hand the model torn content, then record an mtime for that non-state.
// Asserting on the lock itself is deterministic; racing an actual torn read
// would not be.
func TestReadToolTakesPerPathLock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewFileTracker()
	rt, matErr := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if matErr != nil {
		t.Fatal(matErr)
	}

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
		if err != nil {
			t.Fatalf("read failed once the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read never completed after the lock was released")
	}
}

func TestReadToolLineTruncation(t *testing.T) {
	dir := t.TempDir()
	// One 10 KB line, then two normal lines.
	longLine := strings.Repeat("x", 10*1024)
	content := longLine + "\nline2\nline3\n"
	p := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text

	// The long line must be truncated with the suffix.
	if !strings.Contains(out, "… (line truncated)") {
		t.Fatalf("expected line truncation suffix, got:\n%q", out)
	}
	// The suffix must appear on line 1.
	if !strings.Contains(out, fmt.Sprintf("%6d\t", 1)) {
		t.Fatalf("expected line 1 to be present, got:\n%q", out)
	}
	// Lines 2 and 3 must be intact.
	if !strings.Contains(out, "\n"+fmt.Sprintf("%6d\t%s\n", 2, "line2")) {
		t.Fatalf("expected line 2 intact, got:\n%q", out)
	}
	if !strings.Contains(out, "\n"+fmt.Sprintf("%6d\t%s\n", 3, "line3")) {
		t.Fatalf("expected line 3 intact, got:\n%q", out)
	}
}

func TestReadToolByteBudget(t *testing.T) {
	dir := t.TempDir()
	// Build a file large enough to exceed 50 KB.
	var lines []string
	for i := 1; i <= 800; i++ {
		lines = append(lines, fmt.Sprintf("line-%04d-%s", i, strings.Repeat("x", 100)))
	}
	p := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text

	// The output must be roughly <= 50 KB.
	if len(out) > 52*1024 {
		t.Fatalf("output too large: %d bytes", len(out))
	}

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
	if _, scanErr := fmt.Sscanf(lastLine, "[showing lines %d-%d; more lines remain — pass offset=%d to continue]", new(int), new(int), &offset); scanErr != nil {
		t.Fatalf("could not parse offset from %q: %v", lastLine, scanErr)
	}
	// The offset must be > 0 and correspond to the next line.
	if offset <= 1 {
		t.Fatalf("expected offset > 1, got %d", offset)
	}
}

func TestReadToolBinaryDetection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "binary.bin")
	// File with a NUL byte early.
	data := []byte("hello\x00world\n")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	if err == nil {
		t.Fatal("expected an error for binary file")
	}
	if !strings.Contains(err.Error(), "binary") && !strings.Contains(err.Error(), "Binary") {
		t.Fatalf("expected error to mention binary, got %v", err)
	}
}

func TestReadToolUnderBudgetsUnchanged(t *testing.T) {
	// A small file under all budgets must produce byte-identical output to
	// the existing behaviour.
	dir := t.TempDir()
	p := filepath.Join(dir, "small.txt")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	want := fmt.Sprintf("%6d\t%s\n%6d\t%s\n%6d\t%s\n", 1, "line1", 2, "line2", 3, "line3")
	if out != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
}

func TestReadToolRoundTripContinueOffset(t *testing.T) {
	dir := t.TempDir()
	// Build a file larger than 50 KB so the first read is byte-capped.
	var lines []string
	for i := 1; i <= 1000; i++ {
		lines = append(lines, fmt.Sprintf("L%06d:%s", i, strings.Repeat("y", 80)))
	}
	p := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewFileTracker()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: ""})
	if err != nil {
		t.Fatal(err)
	}

	var seenLines []string
	offset := 0
	for {
		offset++
		input := fmt.Sprintf(`{"path":%q}`, p)
		if offset > 1 {
			input = fmt.Sprintf(`{"path":%q,"offset":%d}`, p, offset)
		}
		res, err := rt.Execute(context.Background(), ToolInput(input))
		if err != nil {
			t.Fatal(err)
		}
		out := res.Text

		// Parse emitted lines (skip the continuation trailer).
		rawLines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		nextOffset := 0
		for _, line := range rawLines {
			if strings.HasPrefix(line, "[showing lines") {
				fmt.Sscanf(line, "[showing lines %d-%d; more lines remain — pass offset=%d to continue]", new(int), new(int), &nextOffset)
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
		if seen[l] {
			t.Fatalf("duplicate line: %q", l)
		}
		seen[l] = true
	}

	// We should have seen many lines.
	if len(seenLines) < 100 {
		t.Fatalf("expected at least 100 unique lines from round-trip, got %d", len(seenLines))
	}

	// Verify no gap: the first line should be L000001.
	if !strings.HasPrefix(seenLines[0], "L000001:") {
		t.Fatalf("first line should be L000001, got %q", seenLines[0])
	}
	// The last line should be L001000 (end of file).
	if !strings.HasPrefix(seenLines[len(seenLines)-1], "L001000:") {
		t.Fatalf("last line should be L001000, got %q", seenLines[len(seenLines)-1])
	}
}
