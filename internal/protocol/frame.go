// Package protocol — JSONL framing primitives.
//
// Frames are newline-delimited. The only byte that splits frames is '\n' (0x0A).
// Unicode line-separator U+2028 and paragraph-separator U+2029 are NOT split
// points; they appear as multi-byte UTF-8 sequences inside JSON strings and
// must be left intact. Trailing '\r' before '\n' is accepted and stripped.

package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ErrFrameTooLarge is returned when an incoming frame exceeds the configured
// maximum byte count before a '\n' is seen.
var ErrFrameTooLarge = errors.New("frame too large")

// FrameReader reads LF-terminated frames from an underlying reader, enforcing
// a hard per-frame size cap. It only splits on '\n'; Unicode line-separator
// characters inside JSON payload are not treated as frame boundaries.
type FrameReader struct {
	r        *bufio.Reader
	maxBytes int
	buf      bytes.Buffer
}

// NewFrameReader creates a FrameReader that reads from r and rejects any
// single frame larger than maxBytes.
func NewFrameReader(r io.Reader, maxBytes int) *FrameReader {
	return &FrameReader{
		r:        bufio.NewReaderSize(r, 64*1024),
		maxBytes: maxBytes,
	}
}

// ReadFrame returns the next JSONL frame with the trailing '\n' (and optional
// '\r') removed. It returns (nil, io.EOF) only when the underlying reader is
// exhausted and no partial data remained. A partial frame at EOF (no trailing
// newline) is returned as a final frame on one call, then io.EOF on the next.
func (f *FrameReader) ReadFrame() ([]byte, error) {
	f.buf.Reset()
	for {
		chunk, err := f.r.ReadSlice('\n')
		if len(chunk) > 0 {
			if f.buf.Len()+len(chunk) > f.maxBytes {
				return nil, ErrFrameTooLarge
			}
			f.buf.Write(chunk)
		}
		if err == bufio.ErrBufferFull {
			// Internal buffer filled before '\n' — keep accumulating.
			continue
		}
		if err == nil {
			// Complete frame ending in '\n'.
			line := f.buf.Bytes()
			line = line[:len(line)-1] // drop trailing \n
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			out := make([]byte, len(line))
			copy(out, line)
			return out, nil
		}
		if err == io.EOF {
			if f.buf.Len() > 0 {
				// Partial frame without a terminating newline — return it once,
				// then the caller will get io.EOF on the next call.
				line := f.buf.Bytes()
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				out := make([]byte, len(line))
				copy(out, line)
				f.buf.Reset()
				return out, nil
			}
			return nil, io.EOF
		}
		return nil, err
	}
}

// WriteFrame writes one JSONL frame: payload followed by '\n'.
// payload must not itself contain '\n'.
func WriteFrame(w io.Writer, payload []byte) error {
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}
