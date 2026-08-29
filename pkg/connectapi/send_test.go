// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
)

// fakeAccepter is the narrow inbox seam the Server holds: it records what Send
// submitted and hands back an id. Delivery is the Queue's job and is not what
// these tests are about.
type fakeAccepter struct{ got inbox.Inbound }

func (f *fakeAccepter) Accept(_ context.Context, in inbox.Inbound) (string, error) {
	f.got = in
	return "m_1", nil
}

func textBlocks(s string) []*rafikiv1.ContentBlock {
	return []*rafikiv1.ContentBlock{{
		Index: 0,
		Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: s}},
	}}
}

func TestSendRoutesPromptThroughInbox(t *testing.T) {
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)

	resp, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("hello"),
	}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Msg.GetMessageId() == "" {
		t.Error("SendResponse.MessageId is empty")
	}
	if acc.got.ChildID != "c_1" || acc.got.Mode != inbox.ModePrompt || acc.got.Text != "hello" {
		t.Errorf("inbound = %+v, want child=c_1 mode=prompt text=hello", acc.got)
	}
}

func TestSendMapsSteerAndAbort(t *testing.T) {
	cases := []struct {
		wire rafikiv1.SendMode
		want inbox.Mode
	}{
		{rafikiv1.SendMode_SEND_MODE_STEER, inbox.ModeSteer},
		{rafikiv1.SendMode_SEND_MODE_ABORT, inbox.ModeAbort},
	}
	for _, tc := range cases {
		acc := &fakeAccepter{}
		s := connectapi.NewServer(nil)
		s.SetInbox(acc)
		if _, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
			ChildId: "c_1", Mode: tc.wire, Blocks: textBlocks("x"),
		})); err != nil {
			t.Fatalf("Send(%v): %v", tc.wire, err)
		}
		if acc.got.Mode != tc.want {
			t.Errorf("mode for %v = %v, want %v", tc.wire, acc.got.Mode, tc.want)
		}
	}
}

func TestSendWithoutInboxFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("x"),
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestSendRejectsEmptyChildID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("x"),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestSendRejectsUnspecifiedMode(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Blocks: textBlocks("x"),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestSendRejectsNonTextBlocks proves an image is refused rather than silently
// dropped: Engine.HandlePrompt takes a string, so there is nowhere for image
// bytes to go until that changes.
func TestSendRejectsNonTextBlocks(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{MediaType: "image/png"}},
		}},
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v, want Unimplemented", connect.CodeOf(err))
	}
}

// TestSendAbortNeedsNoBlocks: an abort carries no content.
func TestSendAbortNeedsNoBlocks(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	if _, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Mode: rafikiv1.SendMode_SEND_MODE_ABORT,
	})); err != nil {
		t.Errorf("Send(ABORT) with no blocks: %v", err)
	}
}
