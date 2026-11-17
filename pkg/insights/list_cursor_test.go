// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestListCursorRoundTrip pins the wire shape: base64url of the JSON payload,
// no padding, with the timestamp riding the recent and created sorts and the
// score riding relevance.
func TestListCursorRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 800000000, time.UTC)

	raw := encodeListCursor(ListSortRecent, "", "0b1e2f3a-1111-4222-8333-444455556666", &at, nil)
	cs, err := decodeListCursor(raw, ListSortRecent, "")
	c.Require().NoError(err, "decode recent cursor")
	c.Eq("0b1e2f3a-1111-4222-8333-444455556666", cs.ID, "row id")
	c.Require().True(cs.T != nil, "recent cursor carries a timestamp")
	c.True(cs.T.Equal(at), "timestamp = %v, want %v", cs.T, at)
	c.True(cs.F == nil, "recent cursor carries a score %v, want none", cs.F)
	c.Eq(int(ListSortRecent), cs.S, "sort")
	c.Eq("", cs.P, "parent")

	created := encodeListCursor(ListSortCreated, "parent-1", "0b1e2f3a-1111-4222-8333-444455556666", &at, nil)
	cs, err = decodeListCursor(created, ListSortCreated, "parent-1")
	c.Require().NoError(err, "decode created cursor")
	c.Eq("parent-1", cs.P, "parent")

	score := 3.25
	raw = encodeListCursor(ListSortRelevance, "", "0b1e2f3a-1111-4222-8333-444455556666", nil, &score)
	cs, err = decodeListCursor(raw, ListSortRelevance, "")
	c.Require().NoError(err, "decode relevance cursor")
	c.Require().True(cs.F != nil, "relevance cursor carries a score")
	c.Eq(score, *cs.F, "score")
	c.True(cs.T == nil, "relevance cursor carries a timestamp %v, want none", cs.T)

	// No padding anywhere in the encoding.
	c.NotStrContains(raw, "=", "padded base64")
}

// TestListCursorRejectsTampering pins that undecodable input and payloads that
// do not describe this request are ErrBadCursor, never a silent page.
func TestListCursorRejectsTampering(t *testing.T) {
	c := assert.NewCollecting(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	recent := encodeListCursor(ListSortRecent, "p-X", "0b1e2f3a-1111-4222-8333-444455556666", &at, nil)

	for _, bad := range []string{"", "not base64!!", "zxzxzx"} {
		_, err := decodeListCursor(bad, ListSortRecent, "p-X")
		c.ErrorIs(err, ErrBadCursor, "decode %q", bad)
	}

	// Valid base64 around a payload that is not the cursor's JSON.
	_, err := decodeListCursor("e30=", ListSortRecent, "p-X") // "{}"
	c.ErrorIs(err, ErrBadCursor, "decode {} payload")

	// A cursor minted for the recent sort cannot page a created request.
	_, err = decodeListCursor(recent, ListSortCreated, "p-X")
	c.ErrorIs(err, ErrBadCursor, "recent cursor on a created request")

	// A cursor minted for parent X cannot page parent Y.
	_, err = decodeListCursor(recent, ListSortRecent, "p-Y")
	c.ErrorIs(err, ErrBadCursor, "parent X cursor on parent Y request")

	brokenID := encodeListCursor(ListSortRecent, "p-X", "not-a-uuid", &at, nil)
	_, err = decodeListCursor(brokenID, ListSortRecent, "p-X")
	c.ErrorIs(err, ErrBadCursor, "cursor row id is not a uuid")

	// Spellings uuid.Parse tolerates but Postgres' ::uuid cast refuses must
	// fail the cursor too, not surface a SQL error mid-page.
	for _, spelling := range []string{
		"urn:uuid:0b1e2f3a-1111-4222-8333-444455556666",
		"{0b1e2f3a-1111-4222-8333-444455556666}",
		"0b1e2f3a111142228333444455556666",
	} {
		cs := encodeListCursor(ListSortRecent, "p-X", spelling, &at, nil)
		_, err = decodeListCursor(cs, ListSortRecent, "p-X")
		c.ErrorIs(err, ErrBadCursor, "cursor row id %q is not canonical", spelling)
	}

	// A canonical uuid spelled in upper case is the same uuid: the case is
	// normalised and the cursor decodes, carrying the id through to the
	// keyset, which Postgres casts in any case.
	upperID := strings.ToUpper("0b1e2f3a-1111-4222-8333-444455556666")
	upper := encodeListCursor(ListSortRecent, "p-X", upperID, &at, nil)
	cs, err := decodeListCursor(upper, ListSortRecent, "p-X")
	c.Require().NoError(err, "decode an upper-case cursor id")
	c.Eq(upperID, cs.ID, "the upper-case id rides through")

	// A recent cursor without its timestamp cannot keyset the request.
	noTime := encodeListCursor(ListSortRecent, "p-X", "0b1e2f3a-1111-4222-8333-444455556666", nil, nil)
	_, err = decodeListCursor(noTime, ListSortRecent, "p-X")
	c.ErrorIs(err, ErrBadCursor, "recent cursor without a timestamp")

	noScore := encodeListCursor(ListSortRelevance, "", "0b1e2f3a-1111-4222-8333-444455556666", nil, nil)
	_, err = decodeListCursor(noScore, ListSortRelevance, "")
	c.ErrorIs(err, ErrBadCursor, "relevance cursor without a score")
}
