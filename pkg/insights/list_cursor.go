// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// listCursorState is the JSON payload carried by a ListConversations cursor:
// base64url (no padding) of these fields. S and P bind the cursor to one
// request shape, so a cursor minted for one sort or parent cannot silently
// page another. T carries the sort timestamp for the recent and created
// sorts, F the BM25 score for the relevance sort.
type listCursorState struct {
	S  int        `json:"s"`
	P  string     `json:"p"`
	T  *time.Time `json:"t,omitempty"`
	F  *float64   `json:"f,omitempty"`
	ID string     `json:"id"`
}

// encodeListCursor renders one page boundary. at is set for the recent and
// created sorts, score for the relevance sort.
func encodeListCursor(sort ListSort, parent, id string, at *time.Time, score *float64) string {
	b, err := json.Marshal(listCursorState{S: int(sort), P: parent, T: at, F: score, ID: id})
	if err != nil {
		// Marshalling this struct cannot fail, but an empty cursor is the
		// safe shape: the caller treats it as "no next page".
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeListCursor parses raw and checks it against this request: it must
// decode, carry the request's sort and parent, name a parseable row id, and
// carry the field its sort keys on. Any failure is ErrBadCursor.
func decodeListCursor(raw string, sort ListSort, parent string) (listCursorState, error) {
	var cs listCursorState
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cs, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	if err := json.Unmarshal(b, &cs); err != nil {
		return cs, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	if cs.S != int(sort) {
		return cs, fmt.Errorf("%w: cursor sort %d does not match request sort %d", ErrBadCursor, cs.S, int(sort))
	}
	if cs.P != parent {
		return cs, fmt.Errorf("%w: cursor parent %q does not match request parent %q", ErrBadCursor, cs.P, parent)
	}
	if _, err := uuid.Parse(cs.ID); err != nil {
		return cs, fmt.Errorf("%w: cursor row id %q: %v", ErrBadCursor, cs.ID, err)
	}
	switch sort {
	case ListSortCreated, ListSortRecent:
		if cs.T == nil {
			return cs, fmt.Errorf("%w: cursor carries no timestamp", ErrBadCursor)
		}
	case ListSortRelevance:
		if cs.F == nil {
			return cs, fmt.Errorf("%w: cursor carries no score", ErrBadCursor)
		}
	}
	return cs, nil
}
