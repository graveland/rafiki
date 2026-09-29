// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// parentReporter binds agent_report and agent_result to one child: it calls
// the same scriptHub code the Connect Report/SetResult verbs run (connect_script.go),
// so an LLM child's report and result follow the exact path a script's does —
// the parent's event buffer with the same coalescing, or its own durable log
// when it has no parent — with no second implementation to drift.
//
// The daemon never defaults a kind: the empty kind is the TOOL's default
// ("message", filled in pkg/fundi/tools before this is called), so what
// arrives here is whatever the child chose.
type parentReporter struct {
	c       *Controller
	childID string
}

var _ tools.ParentReporter = (*parentReporter)(nil)

// newParentReporter binds the reporter to one child. Bound, not read per
// call, for the same reason newControllerSpawner is: a self id passed as an
// argument is a tool argument, and a tool argument is produced by an LLM that
// can be prompt-injected into naming somebody else.
func newParentReporter(c *Controller, childID string) *parentReporter {
	return &parentReporter{c: c, childID: childID}
}

// Report sends one note to the child's parent. The message is a plain string
// at this boundary; it is JSON-encoded here because the hub's data_json is a
// complete JSON value (renderJSONPayload decodes a JSON string back to the
// raw text for the parent's frame). The cap is on the ENCODED bytes, the same
// cap the Connect verb enforces — refused, never truncated.
func (p *parentReporter) Report(ctx context.Context, kind, message string) error {
	enc, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode report message: %w", err)
	}
	if len(enc) > connectapi.MaxReportDataBytes {
		return fmt.Errorf("message is %d bytes encoded; the limit is %d",
			len(enc), connectapi.MaxReportDataBytes)
	}
	return scriptHub{c: p.c}.Report(ctx, p.childID, kind, string(enc))
}

// SetResult records the child's result for its next settle. Same shape as
// Report: JSON-encode the string, enforce the Connect verb's cap on the
// encoded bytes, then hand it to the hub the Connect verb runs.
func (p *parentReporter) SetResult(ctx context.Context, result string) error {
	enc, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if len(enc) > connectapi.MaxResultBytes {
		return fmt.Errorf("result is %d bytes encoded; the limit is %d",
			len(enc), connectapi.MaxResultBytes)
	}
	return scriptHub{c: p.c}.SetResult(ctx, p.childID, string(enc))
}
