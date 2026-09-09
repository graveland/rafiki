// SPDX-License-Identifier: Apache-2.0

package conversationview

import (
	"fmt"
	"io"

	"go.graveland.dev/rafiki/pkg/table"
)

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

func (e *errWriter) println(args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintln(e.w, args...)
}

// Write makes errWriter an io.Writer so table builders can render straight
// into it; the captured error short-circuits later writes.
func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// render writes a table through the captured writer, recording its error,
// then terminates the line: lipgloss renders without a trailing newline and
// the next title or summary must start on its own line.
func (e *errWriter) render(b *table.Builder) {
	if e.err != nil {
		return
	}
	e.err = b.Render()
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintln(e.w)
}
