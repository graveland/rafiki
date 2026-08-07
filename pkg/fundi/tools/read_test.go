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
