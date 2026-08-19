// SPDX-License-Identifier: Apache-2.0

package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func decodeErrFrame(t *testing.T, frame []byte) protocol.Response {
	t.Helper()
	var resp protocol.Response
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("response carries no error body: %s", frame)
	}
	return resp
}

// The shape a real pgx connection failure takes. Verified against pgx v5:
// pgx names the database user and database in its own text and the HOST
// arrives via the wrapped resolver error underneath. (It redacts the password
// itself — the leak is infrastructure topology, not credentials.)
const pgxConnFailure = "failed to connect to `user=rafiki_user database=conversations`: " +
	"hostname resolving error: lookup db.internal.example: no such host"

func TestMapErrDoesNotForwardAnErrorItDidNotAuthor(t *testing.T) {
	frame := mapErr("ctrl_conversation_stats", "1", errors.New(pgxConnFailure), protocol.ErrInternal)
	resp := decodeErrFrame(t, frame)

	if resp.Success {
		t.Fatal("error response reports success")
	}
	if resp.Error.Code != protocol.ErrInternal {
		t.Errorf("code = %q, want %q", resp.Error.Code, protocol.ErrInternal)
	}
	for _, leak := range []string{"db.internal.example", "rafiki_user", "conversations", "hostname resolving"} {
		if strings.Contains(resp.Error.Message, leak) {
			t.Errorf("message leaks %q: %q", leak, resp.Error.Message)
		}
	}
}

// The counterweight: an error this codebase authored keeps its text, because
// otherwise every message degrades to "internal error" and the fix costs more
// than the leak did.
func TestMapErrForwardsAControllerErrorVerbatim(t *testing.T) {
	frame := mapErr("ctrl_kill", "1", &ControllerError{
		Code:    protocol.ErrChildNotFound,
		Message: "no child named zoe",
	}, protocol.ErrInternal)
	resp := decodeErrFrame(t, frame)

	if resp.Error.Code != protocol.ErrChildNotFound {
		t.Errorf("code = %q, want %q", resp.Error.Code, protocol.ErrChildNotFound)
	}
	if resp.Error.Message != "no child named zoe" {
		t.Errorf("message = %q, want it forwarded verbatim", resp.Error.Message)
	}
}
