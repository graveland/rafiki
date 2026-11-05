// SPDX-License-Identifier: Apache-2.0

package capture

// testdata/claude-capture-delivery is a REAL Claude Code 2.1.290 session
// recorded through `rafikid proxy record`, chosen for the delivery
// acknowledgement's hard cases: a message sent while a turn was running, and
// two DELIBERATE sends of the same text.
//
// It is here because the acknowledgement's premise is a claim about what the
// captured request contains, and only a real capture can settle it:
//
//   - the mid-turn message does not appear as the user typed it. Claude Code
//     folds it into the running turn wrapped in its own prose ("The user sent a
//     new message while you were working: …"), so a client that matched the
//     captured text against what it sent would retire the wrong row. The
//     receipt names the INBOX ROW instead, and this fixture is the evidence
//     that it must.
//   - two identical sends are two messages. The capture stores two user nodes
//     with identical text; nothing deduplicates them, because nothing may:
//     the uuid, not the text, is the identity.
//   - the echo the daemon acknowledges (`isReplay`, the frame's `uuid`) never
//     reaches the conversation tree. It is stdio, not prompt content, and a
//     captured request must carry no trace of it.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

const deliveryCorpus = "../../testdata/claude-capture-delivery"

// TestDeliveryCorpusKeepsTheMidTurnWrapperAndBothIdenticalSends replays the
// recorded session and asserts the three properties above on the STORED tree.
func TestDeliveryCorpusKeepsTheMidTurnWrapperAndBothIdenticalSends(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "delivery-corpus")

	replayCorpus(t, ctx, s, convID, ref, deliveryCorpus, nil)

	// Every user-role node in the tree, oldest first, with its stored text.
	rows, err := pool.Query(ctx, `
		SELECT role::text, content::text
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND role = 'user'
		 ORDER BY ordinal`, convID)
	c.NoError(err, "read the stored user rows")
	defer rows.Close()

	var texts []string
	for rows.Next() {
		var role, content string
		c.NoError(rows.Scan(&role, &content), "scan a user row")
		texts = append(texts, flattenUserContent(t, content))
	}
	c.NoError(rows.Err(), "user rows")

	// 1. The mid-turn message is stored WRAPPED, and the wrapper carries the
	// user's own words inside it — which is exactly why matching on the stored
	// text cannot identify what was sent.
	var wrapped []string
	for _, tx := range texts {
		if strings.Contains(tx, "while you were working") {
			wrapped = append(wrapped, tx)
		}
	}
	c.Require().Eq(1, len(wrapped), "the recorded session has exactly one mid-turn message; stored user rows were:\n%s", strings.Join(texts, "\n---\n"))
	c.StrContains(wrapped[0], "Reply with exactly one word: also", "the wrapper keeps the user's original words")
	c.NotEq("Reply with exactly one word: also", strings.TrimSpace(wrapped[0]),
		"the mid-turn message is stored verbatim; the wrapper is the reason a text match fails")

	// 2. The two deliberate identical sends are two stored messages, both
	// unwrapped (they were sent after the running turn finished).
	var twice int
	for _, tx := range texts {
		if strings.TrimSpace(tx) == "Reply with exactly one word: twice" {
			twice++
		}
	}
	c.Eq(2, twice, "the two identical sends must be two stored messages; rows:\n%s", strings.Join(texts, "\n---\n"))

	// 3. No echo reached the tree. A captured request never carries the uuid or
	// the isReplay marker — they are siblings of `message` on the stdio frame,
	// below the API.
	var leaks int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid
		   AND (content::text ILIKE '%isReplay%' OR content::text ILIKE '%"uuid"%')`,
		convID).Scan(&leaks), "scan for echo leakage")
	c.Eq(0, leaks, "a stored row carries the stdio echo's uuid/isReplay fields")

	// The first prompt appears exactly once. Together with the two counts above
	// (one wrapper, two identical sends) that accounts for all four recorded
	// messages exactly once: a lost request or a re-stored history would move
	// one of these numbers.
	var ready int
	for _, tx := range texts {
		if strings.Contains(tx, "Then reply with exactly one word: ready") {
			ready++
		}
	}
	c.Eq(1, ready, "the first prompt appears once; rows:\n%s", strings.Join(texts, "\n---\n"))
}

// flattenUserContent renders a stored user node's content as text, joining text
// blocks with newlines. Tool results and images carry no text and contribute
// nothing, which is what the assertions above want: they are about what the
// USER's messages say.
func flattenUserContent(t *testing.T, content string) string {
	t.Helper()
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(content), &blocks); err != nil {
		var s string
		if err2 := json.Unmarshal([]byte(content), &s); err2 == nil {
			return s
		}
		t.Fatalf("decode stored content %q: %v", content, err)
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_result":
			// Claude Code surfaces a mid-turn message "alongside the next tool
			// result", so the wrapper arrives HERE rather than as a user text
			// block — which is exactly the shape a client scanning user text
			// alone would miss.
			if len(b.Content) == 0 {
				continue
			}
			var s string
			if err := json.Unmarshal(b.Content, &s); err == nil {
				parts = append(parts, s)
				continue
			}
			var inner []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(b.Content, &inner); err == nil {
				for _, ib := range inner {
					if ib.Type == "text" && ib.Text != "" {
						parts = append(parts, ib.Text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}
