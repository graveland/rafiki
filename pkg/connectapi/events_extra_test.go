// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
)

// fakeRawChildIO records what each handler passes and answers with injected
// values, so the handlers' success paths, argument pass-through and error
// mapping are all observable without a daemon.
type fakeRawChildIO struct {
	// Recorded arguments.
	streamsChildID string
	streamsWhich   string
	sendChildID    string
	sendFrame      json.RawMessage

	// Injected answers.
	streamsOut *rafikiv1.GetStreamsResponse
	streamsErr error
	sendErr    error
}

func (f *fakeRawChildIO) GetStreams(_ context.Context, childID, which string) (*rafikiv1.GetStreamsResponse, error) {
	f.streamsChildID, f.streamsWhich = childID, which
	if f.streamsErr != nil {
		return nil, f.streamsErr
	}
	return f.streamsOut, nil
}

func (f *fakeRawChildIO) SendFrame(_ context.Context, childID string, frame json.RawMessage) error {
	f.sendChildID, f.sendFrame = childID, frame
	return f.sendErr
}

func newRawChildIOServer(r *fakeRawChildIO) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetRawChildIO(r)
	return s
}

// ─── Success paths ────────────────────────────────────────────────────────────

func TestGetStreamsPassesChildAndWhichThrough(t *testing.T) {
	f := &fakeRawChildIO{streamsOut: &rafikiv1.GetStreamsResponse{
		Alive: true,
		In:    [][]byte{[]byte(`{"type":"user_input"}`)},
	}}
	resp, err := newRawChildIOServer(f).GetStreams(context.Background(),
		connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: "c_1", Which: "in"}))
	if err != nil {
		t.Fatalf("GetStreams: %v", err)
	}
	if f.streamsChildID != "c_1" || f.streamsWhich != "in" {
		t.Errorf("seam got childID=%q which=%q, want c_1/in", f.streamsChildID, f.streamsWhich)
	}
	if !resp.Msg.GetAlive() || len(resp.Msg.GetIn()) != 1 || string(resp.Msg.GetIn()[0]) != `{"type":"user_input"}` {
		t.Errorf("resp = %+v, want the seam answer back", resp.Msg)
	}
}

func TestGetStreamsEmptyWhichMeansAll(t *testing.T) {
	f := &fakeRawChildIO{streamsOut: &rafikiv1.GetStreamsResponse{Alive: false}}
	resp, err := newRawChildIOServer(f).GetStreams(context.Background(),
		connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: "c_1"}))
	if err != nil {
		t.Fatalf("GetStreams: %v", err)
	}
	if f.streamsWhich != "" {
		t.Errorf("seam got which=%q, want the empty value passed through", f.streamsWhich)
	}
	if resp.Msg.GetAlive() {
		t.Error("resp alive = true, want the seam's alive=false (fall back to the on-disk dump)")
	}
}

func TestSendFramePassesChildAndFrameThrough(t *testing.T) {
	f := &fakeRawChildIO{}
	_, err := newRawChildIOServer(f).SendFrame(context.Background(),
		connect.NewRequest(&rafikiv1.SendFrameRequest{ChildId: "c_1", FrameJson: `{"type":"get_state"}`}))
	if err != nil {
		t.Fatalf("SendFrame: %v", err)
	}
	if f.sendChildID != "c_1" || string(f.sendFrame) != `{"type":"get_state"}` {
		t.Errorf("seam got childID=%q frame=%s, want c_1 with the frame verbatim", f.sendChildID, f.sendFrame)
	}
}

// ─── Request-shape validation ─────────────────────────────────────────────────

func TestGetStreamsRequiresChildID(t *testing.T) {
	_, err := newRawChildIOServer(&fakeRawChildIO{}).GetStreams(context.Background(),
		connect.NewRequest(&rafikiv1.GetStreamsRequest{Which: "all"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestGetStreamsRefusesUnknownWhich(t *testing.T) {
	_, err := newRawChildIOServer(&fakeRawChildIO{}).GetStreams(context.Background(),
		connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: "c_1", Which: "bogus"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestSendFrameRequiresChildID(t *testing.T) {
	_, err := newRawChildIOServer(&fakeRawChildIO{}).SendFrame(context.Background(),
		connect.NewRequest(&rafikiv1.SendFrameRequest{FrameJson: `{}`}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// frame_json must parse as a JSON OBJECT before the seam is touched: a JSON
// array and a bare scalar are both InvalidArgument, and the fake must record
// NO call — a rejected shape never reaches the child's stdin.
func TestSendFrameRefusesNonObjectFrameJson(t *testing.T) {
	for _, frame := range []string{"[1]", "nope", `"just a string"`, "42"} {
		t.Run(frame, func(t *testing.T) {
			f := &fakeRawChildIO{}
			_, err := newRawChildIOServer(f).SendFrame(context.Background(),
				connect.NewRequest(&rafikiv1.SendFrameRequest{ChildId: "c_1", FrameJson: frame}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
			}
			if err == nil || !strings.Contains(err.Error(), "frame_json is not a JSON object") {
				t.Errorf("message = %v, want the brief's exact refusal", err)
			}
			if f.sendChildID != "" {
				t.Error("the seam was called; a non-object frame must be refused before it")
			}
		})
	}
}

// The brief's two named cases, pinned verbatim.
func TestSendFrameArrayAndGarbageAreInvalidArgument(t *testing.T) {
	s := newRawChildIOServer(&fakeRawChildIO{})
	for _, frame := range []string{"[1]", "nope"} {
		_, err := s.SendFrame(context.Background(),
			connect.NewRequest(&rafikiv1.SendFrameRequest{ChildId: "c_1", FrameJson: frame}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("frame %q: code = %v, want InvalidArgument", frame, connect.CodeOf(err))
		}
	}
}

// ─── Error mapping ────────────────────────────────────────────────────────────

// The code the daemon attached at the source IS the classification:
// Controller.GetStreams' ErrChildNotFound reads as NotFound with the precise
// reason riding the detail.
func TestGetStreamsUnknownChildIsNotFound(t *testing.T) {
	f := &fakeRawChildIO{streamsErr: &connectapi.ControllerError{
		Code:    protocol.ErrChildNotFound,
		Message: "child not found: c_missing",
	}}
	_, err := newRawChildIOServer(f).GetStreams(context.Background(),
		connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: "c_missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
	if got := rpcreason.Reason(err); got != "child_not_found" {
		t.Errorf("reason = %q, want child_not_found", got)
	}
	if err == nil || !strings.Contains(err.Error(), "child not found: c_missing") {
		t.Errorf("message = %v, want the daemon's authored text", err)
	}
}

// Send's unknown-child refusal maps the same way.
func TestSendFrameUnknownChildIsNotFound(t *testing.T) {
	f := &fakeRawChildIO{sendErr: &connectapi.ControllerError{
		Code:    protocol.ErrChildNotFound,
		Message: "child not found: c_missing",
	}}
	_, err := newRawChildIOServer(f).SendFrame(context.Background(),
		connect.NewRequest(&rafikiv1.SendFrameRequest{ChildId: "c_missing", FrameJson: `{"type":"get_state"}`}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
	if got := rpcreason.Reason(err); got != "child_not_found" {
		t.Errorf("reason = %q, want child_not_found", got)
	}
}

// A generic error — not a connectapi.ControllerError — keeps the blanket Internal, with
// the raw cause redacted and logged (ConnectErr never logs; the handler does).
func TestRawChildIOUncodedErrorIsRedactedInternal(t *testing.T) {
	f := &fakeRawChildIO{sendErr: errors.New("pq: relation does not exist")}
	_, err := newRawChildIOServer(f).SendFrame(context.Background(),
		connect.NewRequest(&rafikiv1.SendFrameRequest{ChildId: "c_1", FrameJson: `{"type":"get_state"}`}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
	if err == nil || strings.Contains(err.Error(), "relation does not exist") {
		t.Errorf("err.Error() = %v, want the raw cause redacted", err)
	}
}

// ─── Unwired fails closed ─────────────────────────────────────────────────────

// SetRawChildIO(nil) is refused, so an explicit nil lands on the handler too:
// the handler-shaped pin newseams_test asks Wave 2 to grow.
func TestRawChildIOUnwiredFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		call func(s *connectapi.Server) error
	}{
		{"GetStreams", func(s *connectapi.Server) error {
			_, err := s.GetStreams(context.Background(), connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: "c_1"}))
			return err
		}},
		{"SendFrame", func(s *connectapi.Server) error {
			_, err := s.SendFrame(context.Background(),
				connect.NewRequest(&rafikiv1.SendFrameRequest{ChildId: "c_1", FrameJson: `{"type":"get_state"}`}))
			return err
		}},
	}
	s := connectapi.NewServer(nil)
	s.SetRawChildIO(nil) // refused, same as never wired
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(s)
			if connect.CodeOf(err) != connect.CodeUnavailable {
				t.Errorf("code = %v, want Unavailable (err = %v)", connect.CodeOf(err), err)
			}
		})
	}
}
