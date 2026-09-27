// SPDX-License-Identifier: Apache-2.0

package eventconv_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestNewVocabularyMessagesExist pins the B0 additions. It asserts only that
// each message and its Event oneof wrapper exist and round-trip their fields —
// the shapes are the contract Task 2 and Task 9 build against.
func TestNewVocabularyMessagesExist(t *testing.T) {
	c := assert.NewCollecting(t)
	ev := &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_ToolExecutionStart{
			ToolExecutionStart: &rafikiv1.ToolExecutionStart{
				ToolUseId: "tu_1",
				Name:      "bash",
			},
		},
	}
	c.Eq("bash", ev.GetToolExecutionStart().GetName(), "ToolExecutionStart.Name")

	end := &rafikiv1.ToolExecutionEnd{ToolUseId: "tu_1", DurationMs: 1500, IsError: true}
	c.False(end.GetDurationMs() != 1500 || !end.GetIsError(), "ToolExecutionEnd round-trip failed: %+v", end)

	retry := &rafikiv1.Retry{Attempt: 2, WillRetry: true, Reason: "overloaded"}
	c.False(retry.GetAttempt() != 2 || !retry.GetWillRetry() || retry.GetReason() != "overloaded", "Retry round-trip failed: %+v", retry)

	spawned := &rafikiv1.ChildSpawned{ChildId: "c_2", ParentId: "c_1", Name: "scout"}
	c.False(spawned.GetParentId() != "c_1" || spawned.GetName() != "scout", "ChildSpawned round-trip failed: %+v", spawned)

	code := int32(3)
	exited := &rafikiv1.ChildExited{ChildId: "c_2", ExitCode: &code, Signal: "SIGKILL"}
	c.False(exited.GetExitCode() != 3 || exited.GetSignal() != "SIGKILL", "ChildExited round-trip failed: %+v", exited)

	// ExitCode is optional: absence must be distinguishable from zero, because
	// exit code 0 means success and "unset" means the child was signalled.
	noCode := &rafikiv1.ChildExited{ChildId: "c_3"}
	c.Nil(noCode.ExitCode, "ChildExited.ExitCode should be nil when unset, got")
}

// TestEventOneofWrappersExist proves each new payload is reachable through the
// Event envelope, which is what StreamEvents actually sends.
func TestEventOneofWrappersExist(t *testing.T) {
	cases := []struct {
		name string
		ev   *rafikiv1.Event
	}{
		{"tool_execution_start", &rafikiv1.Event{Payload: &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: &rafikiv1.ToolExecutionStart{}}}},
		{"tool_execution_end", &rafikiv1.Event{Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{}}}},
		{"retry", &rafikiv1.Event{Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{}}}},
		{"child_spawned", &rafikiv1.Event{Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{}}}},
		{"child_exited", &rafikiv1.Event{Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{}}}},
	}
	for _, tc := range cases {
		assert.NewCollecting(t).NotNil(tc.ev.GetPayload(), "%s: payload is nil", tc.name)
	}
}
