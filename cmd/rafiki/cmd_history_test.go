package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
)

type stubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	events []*rafikiv1.Event
}

func (s *stubControl) GetHistory(
	_ context.Context,
	_ *connect.Request[rafikiv1.GetHistoryRequest],
) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{Events: s.events}), nil
}

func TestRenderHistoryPrintsTextBlocks(t *testing.T) {
	evs := []*rafikiv1.Event{{
		ChildId: "c_1",
		Ordinal: proto.Int32(0),
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hello world"}},
			}},
		}},
	}}

	var sb strings.Builder
	renderHistory(&sb, evs)

	out := sb.String()
	if !strings.Contains(out, "hello world") {
		t.Fatalf("output missing message text:\n%s", out)
	}
	if !strings.Contains(out, "user") {
		t.Fatalf("output missing role label:\n%s", out)
	}
}

func TestHistoryClientRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	path, h := rafikiv1connect.NewControlHandler(&stubControl{events: []*rafikiv1.Event{{
		ChildId: "c_1",
		Ordinal: proto.Int32(7),
	}}})
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(http.DefaultClient, srv.URL)
	resp, err := client.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(resp.Msg.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(resp.Msg.Events))
	}
	if resp.Msg.Events[0].GetOrdinal() != 7 {
		t.Fatalf("ordinal = %d, want 7", resp.Msg.Events[0].GetOrdinal())
	}
}

// serveConnectOnUnixSocket serves a Connect handler over an h2c unix socket
// at path, matching the client half in connectclient.go's connectHTTPClient
// (AllowHTTP h2 over a plain unix dial). Used to prove runHistory actually
// dials a SOCKET profile's own connect.sock rather than some other endpoint.
func serveConnectOnUnixSocket(t *testing.T, path string, handlerPath string, handler http.Handler) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	mux := http.NewServeMux()
	mux.Handle(handlerPath, handler)
	proto := &http.Protocols{}
	proto.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: proto}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})
}

// TestHistoryReachesTheSocketProfilesOwnDaemon pins Fix 4: `rafiki -P
// <socket-profile> history <id>` must dial that profile's own Connect
// socket, not a hardcoded fallback to a production daemon at :8035.
// Before the fix, runHistory read p.URL directly and fell back to
// defaultControlURL ("http://127.0.0.1:8035") for ANY socket profile,
// silently ignoring which daemon the profile actually named.
func TestHistoryReachesTheSocketProfilesOwnDaemon(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// A short directory, not t.TempDir(): unix socket paths are capped at
	// ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests under the
	// full test name, which alone can exceed that.
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")
	connectSock := filepath.Join(dir, "connect.sock") // sibling, per connectSocketFor

	routePath, handler := rafikiv1connect.NewControlHandler(&stubControl{events: []*rafikiv1.Event{{
		ChildId: "c_1",
		Ordinal: proto.Int32(42),
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "served by the profile's own socket"}},
			}},
		}},
	}}})
	serveConnectOnUnixSocket(t, connectSock, routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: controlSock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	cmd := newHistoryCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rafiki history: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "served by the profile's own socket") {
		t.Fatalf("history output does not show the event served by the profile's OWN socket (got):\n%s", out)
	}
}
