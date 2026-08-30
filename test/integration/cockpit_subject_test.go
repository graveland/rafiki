// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// connectClient dials the daemon's second unix socket, where the Connect
// control plane is served as h2c. It must match cmd/rafiki/connectclient.go.
func (d *daemon) connectClient() rafikiv1connect.ControlClient {
	sock := filepath.Join(filepath.Dir(d.socketPath), "connect.sock")
	h := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		},
	}}
	return rafikiv1connect.NewControlClient(h, "http://connect.rafiki.invalid")
}

// spawnChildUnder spawns a child recording parentID as its tree edge.
func (d *daemon) spawnChildUnder(t *testing.T, parentID string) string {
	t.Helper()
	frame := fmt.Sprintf(
		`{"type":"ctrl_spawn","id":"spawnkid","cwd":"/tmp","noSession":true,"kind":"fundi",`+
			`"model":"anthropic/sonnet-latest","parentChildId":%q}`, parentID)
	var r protocol.Response
	mustUnmarshal(t, d.request(t, frame), &r)
	if !r.Success {
		t.Fatalf("ctrl_spawn under %s failed: %+v", parentID, r.Error)
	}
	var data protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &data)
	if data.ChildID == "" {
		t.Fatal("spawn returned empty childId")
	}
	return data.ChildID
}

// The cockpit attached to a child subscribes to subtree + include_self. Without
// include_self, eventlog.ScopeSubtree omits the attached child itself: the rail
// hears about every descendant and never about the one you attached to. That is
// invisible while you look at it, because the focus stream is child-scoped, and
// shows up only as a frozen row after the first hop.
//
// This exercises the real daemon binary, so it covers the whole path the unit
// tests fake: proto field -> filterFromRequest -> eventlog.Subject.match.
func TestIntegration_SubtreeIncludeSelfCoversRootAndDescendant(t *testing.T) {
	d := bootDaemon(t)
	defer d.stopDaemon()

	parent := d.spawnChild(t)
	kid := d.spawnChildUnder(t, parent)

	client := d.connectClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Replay from before the beginning: ordinal 0 is a real event and the log's
	// Read is exclusive on afterOrdinal, so -1 is what "everything" means.
	cursor := &rafikiv1.EventCursor{Ordinals: map[string]int32{parent: -1, kid: -1}}

	for _, tc := range []struct {
		name        string
		includeSelf bool
		wantRoot    bool
	}{
		{"without include_self the root is absent", false, false},
		{"with include_self the root is present", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
			defer scancel()

			stream, err := client.StreamEvents(sctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
				Subject: &rafikiv1.EventSubject{
					Scope:       &rafikiv1.EventSubject_Subtree{Subtree: parent},
					IncludeSelf: tc.includeSelf,
				},
				Tier:   rafikiv1.EventTier_EVENT_TIER_DURABLE,
				Cursor: cursor,
			}))
			if err != nil {
				t.Fatalf("StreamEvents: %v", err)
			}
			defer func() { _ = stream.Close() }()

			seen := map[string]bool{}
			deadline := time.After(8 * time.Second)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for stream.Receive() {
					seen[stream.Msg().GetChildId()] = true
					// Both ids seen, or the descendant seen and the root not
					// expected: nothing more to learn from this subscription.
					if seen[kid] && (seen[parent] || !tc.includeSelf) {
						return
					}
				}
			}()
			select {
			case <-done:
			case <-deadline:
			}

			if !seen[kid] {
				t.Errorf("descendant %s absent from the subtree subscription; seen=%v", kid, seen)
			}
			if seen[parent] != tc.wantRoot {
				t.Errorf("root %s present=%v, want %v (include_self=%v); seen=%v",
					parent, seen[parent], tc.wantRoot, tc.includeSelf, seen)
			}
		})
	}
}

// The rail seeds from ListChildren and reads the parent edge off the labels the
// daemon writes. If that label key ever changes, the rail silently flattens
// into a list of roots rather than failing.
func TestIntegration_ListChildrenCarriesTheParentLabel(t *testing.T) {
	d := bootDaemon(t)
	defer d.stopDaemon()

	parent := d.spawnChild(t)
	kid := d.spawnChildUnder(t, parent)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := d.connectClient().ListChildren(ctx,
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}

	var found bool
	for _, c := range resp.Msg.GetChildren() {
		if c.GetChildId() != kid {
			continue
		}
		found = true
		if got := c.GetLabels()["rafiki/parent"]; got != parent {
			b, _ := json.Marshal(c.GetLabels())
			t.Errorf("rafiki/parent = %q, want %q -- pkg/tui/rail.ParentLabel reads this key; labels=%s",
				got, parent, b)
		}
		if c.LatestOrdinal == nil {
			t.Error("latest_ordinal absent; the rail seeds its clean-board watermark from it")
		}
	}
	if !found {
		t.Fatalf("child %s missing from ListChildren", kid)
	}
}
