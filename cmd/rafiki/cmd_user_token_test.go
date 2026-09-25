package main

// The stale-token dead end and its two unstick paths. A profile token that no
// longer resolves would refuse every framed verb that presents it — including
// `rafiki user create`, the one verb that mints its replacement — so:
//
//   - `user create` dials the LOCAL socket token-less (mustDialWithoutToken;
//     a remote profile keeps its token, since TCP requires it): the UDS is local
//     trust, and the credential's absence is what keeps the recovery verb
//     working.
//   - every other framed verb's refusal names the token file and the recovery,
//     appended centrally at main()'s error print (withTokenAdvice).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// refusingDaemon is a framed fake that mirrors the real UDS server's refusal
// for a credential it does not know: an auth-error ctrl_response, then close.
// Everything else is answered, so the token-less user-create dial gets through.
type refusingDaemon struct {
	mu     sync.Mutex
	firsts []string
}

func (d *refusingDaemon) record(first string) {
	d.mu.Lock()
	d.firsts = append(d.firsts, first)
	d.mu.Unlock()
}

func (d *refusingDaemon) firstType() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.firsts) == 0 {
		return ""
	}
	fields := strings.SplitN(d.firsts[0], " ", 2)
	return fields[0]
}

func (d *refusingDaemon) serve(t *testing.T, sockPath string) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serveConn(conn)
		}
	}()
}

func (d *refusingDaemon) serveConn(conn net.Conn) {
	defer conn.Close()
	r := protocol.NewFrameReader(conn, 1<<20)
	frame, err := r.ReadFrame()
	if err != nil {
		return
	}
	var req struct {
		Type     string `json:"type"`
		ID       string `json:"id"`
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if json.Unmarshal(frame, &req) != nil {
		return
	}
	d.record(req.Type)

	if req.Type == protocol.TypeCtrlAuth {
		// The server's refusal shape (writeAuthError): an error ctrl_response
		// for the auth frame, then the connection closes.
		resp, err := json.Marshal(protocol.Response{
			Type: protocol.TypeCtrlResponse, Command: protocol.TypeCtrlAuth, ID: req.ID,
			Success: false, Error: &protocol.ErrorBody{Code: protocol.ErrAuthInvalid, Message: "invalid auth token"},
		})
		if err == nil {
			_ = protocol.WriteFrame(conn, resp)
		}
		return
	}

	var data any
	switch req.Type {
	case protocol.TypeCtrlUserCreate:
		data = protocol.UserCreateResponseData{
			ID: "u2", Username: req.Username, Token: "rfk_new_minted",
			CreatedAt: "2026-01-01T00:00:00Z",
		}
	default:
		data = struct{}{}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	resp, err := json.Marshal(protocol.Response{
		Type: protocol.TypeCtrlResponse, Command: req.Type, ID: req.ID,
		Success: true, Data: payload,
	})
	if err != nil {
		return
	}
	_ = protocol.WriteFrame(conn, resp)
}

// TestUserCreateDialsWithoutTheProfileToken pins the recovery path: a profile
// whose token no longer resolves must still be able to mint a replacement.
func TestUserCreateDialsWithoutTheProfileToken(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: macOS caps UDS paths at 104 bytes and the
	// test name pushes t.TempDir over it.
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "controller.sock")
	d := &refusingDaemon{}
	d.serve(t, sockPath)
	writeTokenedProfile(t, sockPath, "rfk_stale")

	cmd := newUserCreateCmd()
	cmd.SetArgs([]string{"operator"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("user create failed with a stale profile token: %v\n%s", err, out.String())
	}

	b, err := os.ReadFile(profile.TokenFile("it"))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "rfk_new_minted" {
		t.Fatalf("token file = %q, want the freshly minted token", got)
	}
	if first := d.firstType(); first != protocol.TypeCtrlUserCreate {
		t.Fatalf("first frame = %q, want %q — the stale token must not go on the wire", first, protocol.TypeCtrlUserCreate)
	}
}

func TestWithTokenAdvice(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	dir := t.TempDir()
	writeTokenedProfile(t, filepath.Join(dir, "unused.sock"), "rfk_tok")
	if _, err := resolveProfile(&cobra.Command{}); err != nil {
		t.Fatalf("resolve profile: %v", err)
	}

	authErr := errors.New("client connection closed: auth: auth_invalid: invalid auth token")
	got := withTokenAdvice(authErr)

	msg := got.Error()
	for _, want := range []string{
		profile.TokenFile("it"),
		"delete it",
		"rafiki user create",
		"auth: auth_invalid", // the original reason stays visible
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("advice message %q is missing %q", msg, want)
		}
	}
	if !errors.Is(got, authErr) {
		t.Fatal("the original error is no longer wrapped")
	}

	// An unrelated failure passes through untouched.
	plain := errors.New("boom")
	if withTokenAdvice(plain).Error() != "boom" {
		t.Fatalf("unrelated error was rewritten: %v", withTokenAdvice(plain))
	}

	// No resolved profile (e.g. the failure happened before mustProfile) —
	// formatting must not bootstrap one, and the error passes through.
	resetProfileCache()
	if got := withTokenAdvice(authErr); got.Error() != authErr.Error() {
		t.Fatalf("advice appeared without a resolved profile: %v", got)
	}
}

// A first Request over a refused dial really does carry the auth reason — the
// text withTokenAdvice keys on — so the advice fires on the verb's error, not
// on some hypothetical path.
func TestRefusedDialSurfacesAuthInvalidOnFirstRequest(t *testing.T) {
	dir, err := os.MkdirTemp("", "rafiki")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "controller.sock")
	d := &refusingDaemon{}
	d.serve(t, sockPath)

	c, err := client.DialWithToken(sockPath, "rfk_stale")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.Request(ctx, protocol.StatusRequest{Type: protocol.TypeCtrlStatus})
	if err == nil {
		t.Fatal("a refused dial produced no error")
	}
	if !strings.Contains(err.Error(), "auth: auth_invalid") {
		t.Fatalf("error = %v, want the auth: auth_invalid text withTokenAdvice matches", err)
	}
}
