package main

import (
	"encoding/json"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// labelExecutorID is the label key recording which executor a child landed on.
// Phase 09 needs this for prompt visibility; add it here so phase 09 consumes it.
//
//nolint:unused // wired by full daemon integration (plan-07 task 5)
const labelExecutorID = "rafiki/executor"

// notifyExecutorLost sends a steer event to every child assigned to the given
// executor, telling them their machine is gone.
//
// An executor that is gone for good invalidates the turn its children are
// in the middle of, so this uses a direct send rather than a queued prompt —
// a worker must not spend another 40 seconds believing it still has an executor.
//
//nolint:unused // wired by full daemon integration (plan-07 task 5)
func (c *Controller) notifyExecutorLost(executorID string) {
	for _, snap := range c.st.List() {
		if snap.Labels[labelExecutorID] != executorID || snap.Status == protocol.StatusExited {
			continue
		}
		msg := map[string]any{
			"type":    "ctrl_submit",
			"message": "EXECUTOR LOST: the machine running your file and shell tools is gone and is not coming back. Every read, write, edit and bash call will now fail. Stop what you are doing and report what you had completed.",
		}
		if b, err := json.Marshal(msg); err == nil {
			if err := c.Send(snap.ChildID, b); err != nil {
				slog.Warn("notifyExecutorLost: could not send to child", "childId", snap.ChildID, "executorId", executorID, "error", err)
			}
		}
	}
}
