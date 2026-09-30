package main

import (
	"context"
	"encoding/json"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// buildSendFrame runs spec.Steps through runner as callerID and returns the
// prompt frame carrying the message with the rendered block appended. A send
// with no steps is the plain message, and the runner is never consulted.
func buildSendFrame(ctx context.Context, runner connectapi.SendStepRunner, callerID string, spec tools.SendSpec) (json.RawMessage, tools.SendResult, error) {
	message := spec.Message
	var result tools.SendResult
	if len(spec.Steps) > 0 {
		rendered, summaries, err := runner.RunSendSteps(ctx, callerID, spec.ChildID, spec.Steps)
		if err != nil {
			return nil, tools.SendResult{}, err
		}
		message += "\n\n" + rendered
		result.Steps = summaries
	}
	frame, err := json.Marshal(map[string]string{"type": "prompt", "message": message})
	if err != nil {
		return nil, tools.SendResult{}, err
	}
	return frame, result, nil
}
