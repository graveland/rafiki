package protocol_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"graveland.dev/pi-controller/internal/protocol"
)

func TestFrameReader_SplitsOnLFOnly(t *testing.T) {
	// U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH SEPARATOR)
	// must not split frames — they're valid inside JSON strings.
	input := `{"a":"first\u2028second"}` + "\n" + `{"b":"second\u2029line"}` + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 16*1024*1024)
	var got []string
	for {
		line, err := r.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(line))
	}
	want := []string{
		`{"a":"first\u2028second"}`,
		`{"b":"second\u2029line"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("line %d:\n got  %q\n want %q", i, got[i], want[i])
		}
	}
}

func TestFrameReader_StripsTrailingCR(t *testing.T) {
	input := "line1\r\nline2\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 1024)

	line, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "line1" {
		t.Fatalf("first frame: got %q, want %q", line, "line1")
	}

	line2, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if string(line2) != "line2" {
		t.Fatalf("second frame: got %q, want %q", line2, "line2")
	}
}

func TestFrameReader_LargeFrame(t *testing.T) {
	// 4MB frame must fit when max is 16MB.
	big := strings.Repeat("a", 4*1024*1024)
	input := big + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 16*1024*1024)
	line, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if len(line) != 4*1024*1024 {
		t.Fatalf("got len %d, want 4MB", len(line))
	}
}

func TestFrameReader_FrameTooLarge(t *testing.T) {
	// Set max to 1KB, send 2KB. Should error.
	big := strings.Repeat("a", 2*1024)
	input := big + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 1024)
	_, err := r.ReadFrame()
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameReader_PartialFrameAtEOF(t *testing.T) {
	t.Run("no_cr", func(t *testing.T) {
		input := "partial-no-newline"
		r := protocol.NewFrameReader(strings.NewReader(input), 1024)

		// First call: returns the partial frame without error.
		line, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if string(line) != "partial-no-newline" {
			t.Fatalf("first call: got %q, want %q", line, "partial-no-newline")
		}

		// Second call: clean io.EOF.
		_, err = r.ReadFrame()
		if err != io.EOF {
			t.Fatalf("second call: got %v, want io.EOF", err)
		}
	})

	t.Run("with_cr", func(t *testing.T) {
		// Trailing \r with no \n — CR must be stripped.
		input := "partial-cr\r"
		r := protocol.NewFrameReader(strings.NewReader(input), 1024)

		// First call: CR stripped, content returned.
		line, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if string(line) != "partial-cr" {
			t.Fatalf("first call: got %q, want %q", line, "partial-cr")
		}

		// Second call: clean io.EOF.
		_, err = r.ReadFrame()
		if err != io.EOF {
			t.Fatalf("second call: got %v, want io.EOF", err)
		}
	})
}

func TestFrameWriteRead_RoundTrip(t *testing.T) {
	var b bytes.Buffer
	frames := [][]byte{
		[]byte(`{"type":"prompt","message":"hi"}`),
		[]byte(`{"type":"agent_end"}`),
	}
	for _, f := range frames {
		if err := protocol.WriteFrame(&b, f); err != nil {
			t.Fatal(err)
		}
	}
	r := protocol.NewFrameReader(&b, 1024)
	for i, want := range frames {
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d:\n got  %q\n want %q", i, got, want)
		}
	}
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Fatalf("expected EOF after last frame, got %v", err)
	}
}
