// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
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
			assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "frame is not valid JSON"), "send with frame %q = %v, want the local JSON error", tc.frame, err)
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
	c := assert.NewAborting(t)
	stub := &stubControl{}
	serveStubControl(t, stub)

	const frame = `{"type":"prompt","message":"Hello!"}`
	cmd := newSendCmd()
	_, _, err := runCmd(t, cmd, "c_1", frame)
	c.NoError(err, "rafiki send")
	c.Eq(1, stub.sendCalls, "SendFrame called")
	sent := stub.sendFrames[0]
	if sent.GetChildId() != "c_1" || sent.GetFrameJson() != frame {
		t.Errorf("sent = {child: %q, frame: %q}, want the frame verbatim to c_1", sent.GetChildId(), sent.GetFrameJson())
	}
}

// send keeps the framed-era UX: RangeArgs(0,2), the snd alias, and the
// resolution fallback to the active marker.
func TestSendUxCarriedOver(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newSendCmd()
	c.Require().NotNil(cmd.Args, "send lost its Args validator")
	c.NoError(cmd.Args(cmd, []string{"c_1", "{}"}), "two args must be accepted")
	c.Error(cmd.Args(cmd, []string{"a", "b", "c"}), "three args must be refused")
	if !containsAlias(cmd.Aliases, "snd") {
		t.Errorf("send lost its 'snd' alias: %v", cmd.Aliases)
	}
}
