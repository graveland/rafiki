// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// connectRawChildIO adapts *Controller to connectapi.RawChildIO, the
// debug/operator slice behind the Connect Control service's GetStreams /
// SendFrame RPCs. Each method reproduces what the framed dispatcher's handler
// (pkg/control/dispatch.go's getStreams / ctrlSend) did between decoding its
// frame and writing the response — request validation stays on the connectapi
// side, so an adapter method is pure convert-and-delegate.
//
// The ControllerError values these methods return keep their authored
// messages under their protocol codes: connectapi.ConnectErr maps them, the
// same promise mapErr honors on the framed plane.
type connectRawChildIO struct{ c *Controller }

// GetStreams reads a live child's raw stdin capture via Controller.GetStreams
// — the exact call the framed ctrl_get_streams handler made — and converts
// the framed GetStreamsResponseData onto the proto response, field for field.
// In is never nil: an idle child is an empty repeated field, not a missing
// one. Err is always nil by design (live stderr races the reader goroutine;
// see protocol.GetStreamsResponseData).
func (a connectRawChildIO) GetStreams(_ context.Context, childID, which string) (*rafikiv1.GetStreamsResponse, error) {
	res, err := a.c.GetStreams(childID, which)
	if err != nil {
		return nil, err
	}
	in := make([][]byte, 0, len(res.In))
	in = append(in, res.In...)
	return &rafikiv1.GetStreamsResponse{Alive: res.Alive, In: in, Err: res.Err}, nil
}

// SendFrame forwards a raw child-protocol frame through Controller.Send —
// the exact call the framed ctrl_send handler made. The frame is passed
// through verbatim: Send classifies it (turn frames are durably accepted,
// control frames go straight down) and the interception rules are its
// business, not this adapter's.
func (a connectRawChildIO) SendFrame(_ context.Context, childID string, frame json.RawMessage) error {
	return a.c.Send(childID, frame)
}
