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
	TurnErr           string // non-empty: the turn failed upstream or mid-stream; the capture itself is intact
	// ReleaseOnly marks a report from a request that is NOT the child's own
	// conversation — Claude Code's titler, its quota probe and the per-tool
	// model helpers all ride the session header, and none of them declares a
	// client tool. None of its rows may publish, but the report is still sent,
	// because the response id in it is what releases the child's message gate:
	// a frame that arms the gate and never reports wedges that child's whole
	// event stream until it exits. A sink must release the gate on a
	// release-only report that names a response, and publish nothing.
	ReleaseOnly bool
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
//
// It reports every session request that reached capture — begun OR refused —
// and only those: a request with no session header, no sink or no capture
// outcome belongs to no child, so there is nobody to report to. The request's
// client-tool declaration decides whether its ROWS publish, never whether it
// reports: the report is also the child gate's release, and a request that
// armed the gate and reported nothing left the child's stream silent until it
// exited. See CaptureReport.ReleaseOnly.
func (p *MessagesProxy) reportCapture(r *http.Request, cr captureRef, reqBody []byte) {
	if p.captureSink == nil || cr.session == "" || cr.outcome == nil ||
		(!cr.on && cr.refused == "") {
		return
	}
	errText := cr.refused
	if errText == "" {
		errText = cr.outcome.failure
	}
	releaseOnly := !claudethread.DeclaresClientTools(reqBody)
	var rows []store.Message
	// No read-back for a release-only report: none of its rows can publish, and
	// the read would be a DB round trip on every titler and helper request.
	if cr.on && !releaseOnly {
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
		Rows: rows, Err: errText, TurnErr: cr.outcome.turnFailure, ReleaseOnly: releaseOnly,
	}
	if errText == "" && report.TurnErr == "" {
		report.ResponseMessageID = cr.outcome.responseMessageID
	}
	p.captureSink.CaptureReported(report)
}
