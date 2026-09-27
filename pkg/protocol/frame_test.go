package protocol_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestFrameReader_SplitsOnLFOnly(t *testing.T) {
	c := assert.NewAborting(t)
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
		c.NoError(err)
		got = append(got, string(line))
	}
	want := []string{
		`{"a":"first\u2028second"}`,
		`{"b":"second\u2029line"}`,
	}
	c.Len(got, len(want), "got %d lines, want", len(got))
	for i := range got {
		c.Eq(want[i], got[i], "line %d:\n got  %q\n want", i, got[i])
	}
}

func TestFrameReader_StripsTrailingCR(t *testing.T) {
	c := assert.NewAborting(t)
	input := "line1\r\nline2\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 1024)

	line, err := r.ReadFrame()
	c.NoError(err)
	c.Eq("line1", string(line), "first frame: got %q, want", line)

	line2, err := r.ReadFrame()
	c.NoError(err, "second frame")
	c.Eq("line2", string(line2), "second frame: got %q, want", line2)
}

func TestFrameReader_LargeFrame(t *testing.T) {
	c := assert.NewAborting(t)
	// 4MB frame must fit when max is 16MB.
	big := strings.Repeat("a", 4*1024*1024)
	input := big + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 16*1024*1024)
	line, err := r.ReadFrame()
	c.NoError(err)
	c.Len(line, 4*1024*1024, "got len %d, want 4MB", len(line))
}

func TestFrameReader_FrameTooLarge(t *testing.T) {
	// Set max to 1KB, send 2KB. Should error.
	big := strings.Repeat("a", 2*1024)
	input := big + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 1024)
	_, err := r.ReadFrame()
	assert.NewAborting(t).ErrorIs(err, protocol.ErrFrameTooLarge, "got")
}

func TestFrameReader_PartialFrameAtEOF(t *testing.T) {
	t.Run("no_cr", func(t *testing.T) {
		c := assert.NewAborting(t)
		input := "partial-no-newline"
		r := protocol.NewFrameReader(strings.NewReader(input), 1024)

		// First call: returns the partial frame without error.
		line, err := r.ReadFrame()
		c.NoError(err, "first call")
		c.Eq("partial-no-newline", string(line), "first call: got %q, want", line)

		// Second call: clean io.EOF.
		_, err = r.ReadFrame()
		c.False(err != io.EOF, "second call: got %v, want io.EOF", err)
	})

	t.Run("with_cr", func(t *testing.T) {
		c := assert.NewAborting(t)
		// Trailing \r with no \n — CR must be stripped.
		input := "partial-cr\r"
		r := protocol.NewFrameReader(strings.NewReader(input), 1024)

		// First call: CR stripped, content returned.
		line, err := r.ReadFrame()
		c.NoError(err, "first call")
		c.Eq("partial-cr", string(line), "first call: got %q, want", line)

		// Second call: clean io.EOF.
		_, err = r.ReadFrame()
		c.False(err != io.EOF, "second call: got %v, want io.EOF", err)
	})
}

func TestFrameWriteRead_RoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	var b bytes.Buffer
	frames := [][]byte{
		[]byte(`{"type":"prompt","message":"hi"}`),
		[]byte(`{"type":"agent_end"}`),
	}
	for _, f := range frames {
		c.NoError(protocol.WriteFrame(&b, f))
	}
	r := protocol.NewFrameReader(&b, 1024)
	for i, want := range frames {
		got, err := r.ReadFrame()
		c.NoError(err, "frame %d", i)
		c.True(bytes.Equal(got, want), "frame %d:\n got  %q\n want %q", i, got, want)
	}
	_, err := r.ReadFrame()
	c.False(err != io.EOF, "expected EOF after last frame, got %v", err)
}
