// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// send validates the frame locally: a non-JSON (or non-object) frame is
// refused before anything dials, so a typo never reaches the daemon.
func TestSendRejectsNonJSONBeforeDialling(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	cases := []struct {
		name  string
		frame string
	}{
		{"not json", "not json"},
		{"json array", `["not","an","object"]`},
		{"bare scalar", `"just a string"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubControl{}
			serveStubControl(t, stub)

			cmd := newSendCmd()
			_, _, err := runCmd(t, cmd, "c_1", tc.frame)
			if err == nil || !strings.Contains(err.Error(), "frame is not valid JSON") {
				t.Fatalf("send with frame %q = %v, want the local JSON error", tc.frame, err)
			}
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if stub.sendCalls != 0 || stub.listCalls != 0 {
				t.Fatalf("send dialled the daemon before validating the frame (sendCalls=%d, listCalls=%d)", stub.sendCalls, stub.listCalls)
			}
		})
	}
}

// A valid frame is forwarded verbatim to the resolved child.
func TestSendSendsTheFrameVerbatim(t *testing.T) {
	stub := &stubControl{}
	serveStubControl(t, stub)

	const frame = `{"type":"prompt","message":"Hello!"}`
	cmd := newSendCmd()
	_, _, err := runCmd(t, cmd, "c_1", frame)
	if err != nil {
		t.Fatalf("rafiki send: %v", err)
	}
	if stub.sendCalls != 1 {
		t.Fatalf("SendFrame called %d times, want 1", stub.sendCalls)
	}
	sent := stub.sendFrames[0]
	if sent.GetChildId() != "c_1" || sent.GetFrameJson() != frame {
		t.Errorf("sent = {child: %q, frame: %q}, want the frame verbatim to c_1", sent.GetChildId(), sent.GetFrameJson())
	}
}

// send keeps the framed-era UX: RangeArgs(0,2), the snd alias, and the
// resolution fallback to the active marker.
func TestSendUxCarriedOver(t *testing.T) {
	cmd := newSendCmd()
	if cmd.Args == nil {
		t.Fatal("send lost its Args validator")
	}
	if err := cmd.Args(cmd, []string{"c_1", "{}"}); err != nil {
		t.Errorf("two args must be accepted: %v", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b", "c"}); err == nil {
		t.Error("three args must be refused")
	}
	if !containsAlias(cmd.Aliases, "snd") {
		t.Errorf("send lost its 'snd' alias: %v", cmd.Aliases)
	}
}
