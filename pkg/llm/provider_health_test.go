// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// recordingSink is the durable ejection log the guard writes through: it keeps
// every record in order, so a test can assert what the guard decided and why.
type recordingSink struct {
	mu      sync.Mutex
	records []routing.EjectionRecord
}

func (s *recordingSink) Append(_ context.Context, e routing.EjectionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, e)
	return nil
}

func (s *recordingSink) Active(context.Context, time.Time) ([]routing.EjectionRecord, error) {
	return nil, nil
}

// reasons lists the recorded reasons in the order they were written.
func (s *recordingSink) reasons() []routing.EjectReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]routing.EjectReason, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r.Reason)
	}
	return out
}

// find returns the first recorded ejection of reason.
func (s *recordingSink) find(reason routing.EjectReason) (routing.EjectionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.Reason == reason {
			return r, true
		}
	}
	return routing.EjectionRecord{}, false
}

// transientErr is a 503 the codec classifies as a retryable transport failure:
// the shape a provider's own outage arrives as.
func transientErr() *msg.APIError {
	return &msg.APIError{
		Status:  http.StatusServiceUnavailable,
		Body:    []byte(`{"error":{"type":"api_error","message":"upstream unavailable"}}`),
		Kind:    msg.ErrTransient,
		Dialect: msg.DialectAnthropic,
		Message: "upstream unavailable",
	}
}

// TestFailureKindMapping pins the guard's error vocabulary row by row: only a
// provider's own failure (a 5xx, an overload, a pinned provider's 429, a
// no-eligible rejection) may ever become evidence against it, and the zero kind
// stays FailureNone so an unclassified error bans nobody.
func TestFailureKindMapping(t *testing.T) {
	apiErr := func(kind msg.ErrorKind) error {
		return &msg.APIError{Status: http.StatusBadRequest, Kind: kind, Dialect: msg.DialectAnthropic}
	}
	cases := []struct {
		name   string
		err    error
		pinned bool
		want   routing.FailureKind
	}{
		{"nil", nil, false, routing.FailureNone},
		{"transient 503", apiErr(msg.ErrTransient), false, routing.FailureTransient},
		{"overloaded 529", apiErr(msg.ErrOverloaded), false, routing.FailureTransient},
		{"rate limit, pinned", apiErr(msg.ErrRateLimit), true, routing.FailureTransient},
		{"rate limit, unpinned", apiErr(msg.ErrRateLimit), false, routing.FailureNotProvider},
		{"no eligible, pinned", apiErr(msg.ErrNoEligible), true, routing.FailureNoEligible},
		{"no eligible, unpinned", apiErr(msg.ErrNoEligible), false, routing.FailureNoEligible},
		{"usage limit", apiErr(msg.ErrUsageLimit), true, routing.FailureNotProvider},
		{"prompt too large", apiErr(msg.ErrPromptTooLarge), true, routing.FailureNotProvider},
		{"other 400", apiErr(msg.ErrOther), true, routing.FailureNotProvider},
		{"deadline exceeded", context.DeadlineExceeded, true, routing.FailureTransient},
		{"transport error", errors.New("connection reset by peer"), true, routing.FailureTransient},
		{"wrapped transient", errors.Join(errors.New("send"), apiErr(msg.ErrTransient)), false, routing.FailureTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			c.Eq(tc.want, failureKind(tc.err, tc.pinned), "failureKind(%v, pinned=%v)", tc.err, tc.pinned)
		})
	}
}

// TestToolArgsInvalidCounts pins the tool-corruption rule's evidence: a tool
// call whose input is not a JSON object, or which is missing a key its own
// schema marks required, is one invalid argument set. A tool the request never
// offered, or one whose schema declares no required keys, cannot be judged.
func TestToolArgsInvalidCounts(t *testing.T) {
	c := assert.NewCollecting(t)
	editSchema := json.RawMessage(`{"type":"object","properties":{
		"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},
		"required":["path","old_string","new_string"]}`)
	openSchema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	tools := []msg.ToolDef{
		{Name: "edit", InputSchema: editSchema},
		{Name: "read", InputSchema: openSchema},
	}
	cases := []struct {
		name    string
		toolUse msg.ToolUse
		want    int
	}{
		{"complete input", msg.ToolUse{Name: "edit", Input: json.RawMessage(`{"path":"a","old_string":"b","new_string":"c"}`)}, 0},
		{"missing a required key", msg.ToolUse{Name: "edit", Input: json.RawMessage(`{"path":"a","old_string":"b"}`)}, 1},
		{"input is not an object", msg.ToolUse{Name: "edit", Input: json.RawMessage(`[]`)}, 1},
		{"unknown tool name", msg.ToolUse{Name: "grep", Input: json.RawMessage(`[]`)}, 0},
		{"schema without required", msg.ToolUse{Name: "read", Input: json.RawMessage(`{}`)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.Eq(tc.want, invalidToolArgs(tools, []msg.Block{tc.toolUse}), "invalidToolArgs(%s)", tc.name)
		})
	}
}

// TestRepetitionScoresTextAndThinking pins the degeneracy evidence: short output
// is not scored at all (nil, NOT 0 — 0 means maximal repetition), while a long
// loop spanning a thinking block and a text block scores low enough to ban.
func TestRepetitionScoresTextAndThinking(t *testing.T) {
	c := assert.NewCollecting(t)

	c.Nil(repetition(&msg.Response{Content: []msg.Block{msg.Text{Text: "hello there"}}}),
		"short output must not be scored")
	c.Nil(repetition(&msg.Response{Content: []msg.Block{msg.Text{Text: "hi"}}}),
		"a response with no scoreable block must not be scored")
	c.Nil(repetition(nil), "a nil response must not be scored")

	// One loop, split across the two blocks a real degenerating turn produces.
	loop := strings.Repeat("the same sentence repeats again and again ", 250)
	score := repetition(&msg.Response{Content: []msg.Block{
		msg.Thinking{Text: loop},
		msg.Text{Text: loop},
	}})
	c.Require().NotNil(score, "a 10KB loop across a thinking and a text block must be scored")
	c.Less(0.2, *score, "a repetition loop must score below the degeneracy threshold, got %v", *score)

	// Ordinary long output is scored and stays well clear of the threshold. The
	// words must be distinct: ANY fixed phrase repeated is a loop by this
	// measure, which is exactly the property being pinned.
	var varied strings.Builder
	for i := range 2000 {
		varied.WriteString("token")
		varied.WriteString(strconv.Itoa(i))
		varied.WriteString(" ")
	}
	ok := repetition(&msg.Response{Content: []msg.Block{msg.Text{Text: varied.String()}}})
	c.Require().NotNil(ok, "ordinary long output must be scored")
	c.Greater(0.2, *ok, "ordinary output must score above the degeneracy threshold, got %v", *ok)
}
