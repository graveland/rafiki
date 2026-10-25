// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"time"

	"go.graveland.dev/rafiki/pkg/claudethread"
	"go.graveland.dev/rafiki/pkg/store"
)

// CaptureReport is what one begun capture stored, reported once after the
// proxy's last write for the request.
type CaptureReport struct {
	Session           string // X-Rafiki-Session: the claude child's id
	ThreadID          string // routed thread; "" is the main thread
	ConversationID    string
	ResponseMessageID string // the response's message id; "" when it was not stored
	Rows              []store.Message
	Err               string // non-empty: the capture failed
}

// CaptureSink receives capture reports. Implemented by the daemon's Controller.
type CaptureSink interface {
	CaptureReported(CaptureReport)
}

// SetCaptureSink attaches the report receiver. A nil sink is refused (the
// field stays unset), the same convention as connectapi's Set* setters.
func (p *MessagesProxy) SetCaptureSink(s CaptureSink) {
	if s == nil {
		return
	}
	p.captureSink = s
}

// reportCapture sends the request's CaptureReport. It runs deferred from
// ServeHTTP, after every capture write for the request: none runs on another
// goroutine.
func (p *MessagesProxy) reportCapture(r *http.Request, cr captureRef, reqBody []byte) {
	if p.captureSink == nil || cr.session == "" || cr.outcome == nil ||
		(!cr.on && cr.refused == "") || !claudethread.DeclaresClientTools(reqBody) {
		return
	}
	errText := cr.refused
	if errText == "" {
		errText = cr.outcome.failure
	}
	var rows []store.Message
	if cr.on {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
		defer cancel()
		var rerr error
		rows, rerr = p.store.MessagesSince(ctx, cr.convID, cr.createdAt)
		if rerr != nil {
			rows = nil
			if errText != "" {
				errText += "; "
			}
			errText += "read back failed: " + rerr.Error()
		}
	}
	report := CaptureReport{
		Session: cr.session, ThreadID: cr.threadID, ConversationID: cr.convID,
		Rows: rows, Err: errText,
	}
	if errText == "" {
		report.ResponseMessageID = cr.outcome.responseMessageID
	}
	p.captureSink.CaptureReported(report)
}
