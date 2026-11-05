// SPDX-License-Identifier: Apache-2.0

package integration_test

// The Python SDK's half of the claude delivery acknowledgement: the generated
// codec must DECODE the real `message_delivered` payload a real daemon
// publishes, and the id must survive a cursor replay.
//
// This is not a duplicate of the Go end-to-end test. The SDK has its own
// generated `Event`, its own `from_dict`, and its own stream client, and a
// control-plane payload that decodes in Go can still arrive as `None` in
// Python (a renamed arm, a payload the generator dropped). The Rust half is
// covered by its decoding and follow tests, so this file adds only the Python
// one — and it deliberately reuses the same scratch daemon harness
// (bootSDKDaemon + the profile driver env) rather than inventing a second one.

import (
	"path/filepath"
	"strings"
	"testing"
)

// sdkClaudeDeliveryDriver is the Python side: spawn a claude child on the
// fakeclaude fixture, send a message, and read the receipt off a raw
// StreamEvents feed twice — once live, once replayed from a cursor just below
// the receipt's ordinal.
//
// It uses Client's own stream plumbing (`_conn.stream`) rather than a
// convenience verb because the receipt is an EVENT, not a settle: the SDK has
// no public event feed yet, and inventing one here would be testing a wrapper
// this change does not ship. `_conn` is the SDK's own transport, so the
// envelope, the retry budget and the protocol-epoch gate are all the real
// ones; only the payload decoding is what this driver asserts on.
const sdkClaudeDeliveryDriver = `
import sys, traceback

sys.path.insert(0, "%SDKDIR%")
from rafiki import Client
from rafiki._gen import control_pb, event_pb

probe = sys.argv[sys.argv.index("--probe-out") + 1]
def note(s):
    with open(probe, "a") as f:
        f.write(s + "\n")

def stream(c, cid, cursor):
    req = control_pb.StreamEventsRequest(
        subject=control_pb.EventSubject(child=cid),
        tier="EVENT_TIER_DURABLE",
        cursor=control_pb.EventCursor(ordinals={cid: cursor}),
    )
    return c._conn.stream("StreamEvents", req.to_dict(), read_timeout=60)

def receipt(c, cid, cursor):
    for payload in stream(c, cid, cursor):
        ev = event_pb.Event.from_dict(payload)
        if ev.message_delivered is not None:
            return ev
    raise AssertionError("the stream ended with no message_delivered event")

def run():
    try:
        c = Client.from_profile()
        cid = c.spawn(kind="claude", model="anthropic/sonnet-latest", cwd="/tmp", name="sdk-delivery")
        assert cid.startswith("c_"), cid
        note("spawned %s" % cid)

        row_id = c.send(cid, "the python sdk receipt")
        assert row_id != "", "send returned no inbox row id"

        live = receipt(c, cid, -1)
        assert live.message_delivered.message_id == row_id, (live.message_delivered.message_id, row_id)
        assert live.ordinal is not None, "the receipt arrived without a durable ordinal"
        note("live receipt ordinal=%s message_id=%s" % (live.ordinal, live.message_delivered.message_id))

        # The durable half: re-open from just BELOW the receipt's ordinal (a
        # cursor is exclusive) and the same payload must decode again with the
        # same message id.
        replayed = receipt(c, cid, int(live.ordinal) - 1)
        assert replayed.message_delivered.message_id == row_id, replayed.message_delivered.message_id
        assert replayed.ordinal == live.ordinal, (replayed.ordinal, live.ordinal)
        note("replayed receipt ordinal=%s message_id=%s" % (replayed.ordinal, replayed.message_delivered.message_id))

        c.stop(cid)
        note("PYTHON SDK DELIVERY PASS")
    except Exception:
        note(traceback.format_exc())
        sys.exit(1)

run()
`

// TestPythonSDK_ClaudeDelivery runs the driver above against a real daemon
// whose claude children are the fakeclaude fixture.
func TestPythonSDK_ClaudeDelivery(t *testing.T) {
	t.Parallel()
	py := sdkPython(t)

	// The fixture, and echo-only mode: this test asserts the RECEIPT, not a
	// capture, so no request leaves the daemon.
	events := t.TempDir()
	d := bootSDKDaemon(t, py,
		"CLAUDE_BINARY="+buildFakeClaude(t),
		"FAKE_CLAUDE_DIR="+events,
	)
	_, configDir := scriptUser(t, d.daemon)
	binDir := sdkBinDir(t)

	out := runSDKScript(t, py,
		strings.ReplaceAll(sdkClaudeDeliveryDriver, "%SDKDIR%", filepath.Join(repoRoot, "sdk", "python")),
		sdkDriverEnv(t, configDir, binDir))
	t.Logf("driver output:\n%s", out)
}
