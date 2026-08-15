// SPDX-License-Identifier: Apache-2.0

package conversationview

import "io"

// Mode selects how a result value reaches the user.
type Mode int

const (
	ModeTable       Mode = iota // human-first tables / markdown
	ModeJSON                    // indented JSON
	ModeJSONCompact             // single-line JSON
)

// Render writes v as JSON per m, or delegates to table for ModeTable. Both the
// socket client (`rafiki conversations`) and the DSN-direct CLI (`rafikid
// agent`) route every stats/search/export/findings result through here, so the
// two cannot present the same data differently — they read the same rows
// through the same pkg/insights queries and differ only in transport.
//
// Passing the renderer as func(io.Writer, T) rather than a closure is
// deliberate: the type parameter ties v to the renderer that understands it,
// so a call site cannot pair a value with the wrong table.
func Render[T any](w io.Writer, v T, m Mode, table func(io.Writer, T) error) error {
	switch m {
	case ModeJSON:
		return RenderJSON(w, v, true)
	case ModeJSONCompact:
		return RenderJSON(w, v, false)
	default:
		return table(w, v)
	}
}
