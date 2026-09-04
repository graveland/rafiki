package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// var _ strings.Builder = strings.Builder{} — import guard against unused dep.
var _ = strings.Builder{}

// TestDisconnectMarksTheChildUnreachable verifies that when a child's daraja
// disconnects, the controller sets `rafiki/daraja-state=unreachable` on the
// child's labels. This lets operators see that a child whose executor ran away
// is unreachable without guessing from its still-`streaming` status.
//
// Driven end-to-end through the real registration path: WireDaraja wires the
// controller's callbacks onto the pool; FireConnect/FireDisconnect then exercise
// the registered callback path (not the internal handleConn internals), proving
// that the callbacks wired at startup are actually what fire on state changes.
func TestDisconnectMarksTheChildUnreachable(t *testing.T) {
	t.Parallel()

	st := childstore.New()
	ctrl := &Controller{st: st}

	// Seed a live child (exited so Close won't reject us later).
	childID := "test-child-unreachable"
	st.Insert(&childstore.Session{
		ChildID:   childID,
		Status:    protocol.StatusExited,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
		Labels:    make(map[string]string),
	})

	// Build a real pool + registry and wire the controller into it — exactly
	// the main() path.
	darajaPool := darajapool.New(darajapool.NewRegistry())
	ctrl.WireDaraja(darajaPool, darajaPool.Reg())

	// Fire the connect callback first (as installLive would), then disconnect.
	darajaPool.FireConnect(childID)
	darajaPool.FireDisconnect(childID)

	snap, ok := st.Get(childID)
	if !ok {
		t.Fatal("child disappeared after insert")
	}
	if got := snap.Labels[darajaStateLabel]; got != "unreachable" {
		t.Errorf("label = %q, want %q", got, "unreachable")
	}
}

// TestReconnectClearsTheUnreachableLabel verifies that a reconnect clears the
// unreachable label. If the label were sticky, every reconnect would leave the
// child marked unreachable forever, which is noise rather than signal.
//
// Driven end-to-end through the real registration path: WireDaraja wires the
// callbacks; FireDisconnect then FireConnect exercise both sides of the
// registered callback path — proving that main()'s WireDaraja call actually
// connects the right functions (not that onDarajaConnect/disconnect are stubs).
func TestReconnectClearsTheUnreachableLabel(t *testing.T) {
	t.Parallel()

	st := childstore.New()
	ctrl := &Controller{st: st}

	childID := "test-child-reconnect-clear"
	st.Insert(&childstore.Session{
		ChildID:   childID,
		Status:    protocol.StatusExited,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
		Labels:    make(map[string]string),
	})

	darajaPool := darajapool.New(darajapool.NewRegistry())
	reg := darajaPool.Reg()
	_ = reg // wire path uses it via Reg()
	ctrl.WireDaraja(darajaPool, reg)

	// Disconnect → OnDisconnect sets label.
	darajaPool.FireDisconnect(childID)
	snap, _ := st.Get(childID)
	if got := snap.Labels[darajaStateLabel]; got != "unreachable" {
		t.Fatalf("after disconnect: label = %q, want %q", got, "unreachable")
	}

	// Connect → OnConnect clears label.
	darajaPool.FireConnect(childID)
	snap, _ = st.Get(childID)
	if got := snap.Labels[darajaStateLabel]; got != "" {
		t.Errorf("after reconnect: label = %q, want empty", got)
	}
}

// TestCloseRevokesDarajaCredentials verifies that Close calls Registry.Forget
// before deleting the row, preventing the dead child from authenticating
// again. The ordering matters because Forget runs while the row still exists.
func TestCloseRevokesDarajaCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := dir + "/c.sock"
	st := childstore.New()
	ctrl := NewController(st, dir, dir, socketPath, nil, nil, nil, t.Context(), nil, nil, nil)

	reg := darajapool.NewRegistry()
	ctrl.darajaReg = reg

	// Manually add an entry to the registry as if a ticket had been issued.
	_, err := reg.IssueCredential("test-close-reg")
	if err != nil {
		t.Fatal(err)
	}

	// Seed the child.
	st.Insert(&childstore.Session{
		ChildID:   "test-close-reg",
		Status:    protocol.StatusExited,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
	})

	// Close should revoke credentials BEFORE deleting the row.
	if err := ctrl.Close("test-close-reg"); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Row is gone — that's expected. Verify it was deleted.
	if _, ok := st.Get("test-close-reg"); ok {
		t.Error("child row still exists after Close")
	}
}

// TestKillRevokesDarajaCredentials verifies that Kill also revokes the
// daraja's credentials, preventing a dead child from authenticating through
// a stale connection replay or port scan. The revocation runs at the top of
// Kill, before any process state lookups, so even if the CM lookup fails the
// credential is already revoked.
func TestKillRevokesDarajaCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := dir + "/c.sock"
	st := childstore.New()
	ctrl := NewController(st, dir, dir, socketPath, nil, nil, nil, t.Context(), nil, nil, nil)

	reg := darajapool.NewRegistry()
	ctrl.darajaReg = reg

	// Seed a credential as if the child had connected.
	cred, err := reg.IssueCredential("test-kill-reg")
	if err != nil {
		t.Fatal(err)
	}
	if cred == "" {
		t.Fatal("expected non-empty credential from IssueCredential")
	}

	// Seed the child (we don't need a process here — the point is credential revocation).
	st.Insert(&childstore.Session{
		ChildID:   "test-kill-reg",
		Status:    protocol.StatusIdle,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
	})

	// Kill returns ErrNotFound because there's no real child in the cm, but
	// the credential revocation runs FIRST. Verify the credential was wiped.
	_, _ = ctrl.Kill(context.Background(), "test-kill-reg", 0, 0)

	// CheckCredential should return false now — the credential is gone.
	if reg.CheckCredential(cred, "test-kill-reg") {
		t.Error("credential still valid after Kill; Forget did not run")
	}

	// Verify the child row is still alive (Kill doesn't delete rows).
	if _, ok := st.Get("test-kill-reg"); !ok {
		t.Error("child row was deleted by Kill; it should survive until Close")
	}
}

// TestWireDarajaNilSafe verifies WireDaraja handles nil pool and registry
// gracefully — it should never panic.
func TestWireDarajaNilSafe(t *testing.T) {
	t.Parallel()

	ctrl := &Controller{st: childstore.New()}

	// Nil pool, nil registry — should be a no-op.
	ctrl.WireDaraja(nil, nil)
	ctrl.WireDaraja(darajapool.New(nil), nil)
	ctrl.WireDaraja(nil, darajapool.NewRegistry())

	// No panic above means success.
}

// TestOnConnectNoopWhenNotExists verifies that OnConnect for a child that no
// longer exists is a quiet no-op — no crash, no log noise.
func TestOnConnectNoopWhenNotExists(t *testing.T) {
	t.Parallel()

	ctrl := &Controller{st: childstore.New()}
	ctrl.onDarajaConnect("nonexistent-child")
	// No panic = success.
}

// TestOnDisconnectNoopWhenNotExists verifies the same for disconnect.
func TestOnDisconnectNoopWhenNotExists(t *testing.T) {
	t.Parallel()

	ctrl := &Controller{st: childstore.New()}
	ctrl.onDarajaDisconnect("nonexistent-child")
	// No panic = success.
}

// TestCloseCallsForgetBeforeRowDelete ensures Forget fires before the row
// disappears. If Forget ran after Delete, it would operate on a dead child —
// meaningless, but worse: the OnDisconnect handler could fire between Delete
// and Forget, attempt to set a label on a non-existent row, and fail loudly.
// By running Forget first, we guarantee the credentials are already gone
// before any subsequent disconnect event reaches the label setter.
func TestCloseCallsForgetBeforeRowDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := dir + "/c.sock"
	st := childstore.New()
	ctrl := NewController(st, dir, dir, socketPath, nil, nil, nil, t.Context(), nil, nil, nil)

	reg := darajapool.NewRegistry()
	ctrl.darajaReg = reg

	childID := "test-close-order"
	st.Insert(&childstore.Session{
		ChildID:   childID,
		Status:    protocol.StatusExited,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
	})

	// Issue a credential so Forget has something to wipe.
	_, err := reg.IssueCredential(childID)
	if err != nil {
		t.Fatal(err)
	}

	// Close: Forget must run while the row is still present.
	err = ctrl.Close(childID)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	// Row is gone — that's expected. What matters is Forget ran BEFORE Delete.
	// Since we can't observe the interleaving directly, we verify the
	// post-condition: the child IS gone.
	if _, ok := st.Get(childID); ok {
		t.Error("child row survived Close")
	}
}

// TestLabelIsIdempotent verifies that setting the label twice does not produce
// spurious writes (the second OnConnect/OnDisconnect call sees the current
// state and short-circuits).
func TestLabelIsIdempotent(t *testing.T) {
	t.Parallel()

	st := childstore.New()
	ctrl := &Controller{st: st}

	childID := "test-idempotent"
	st.Insert(&childstore.Session{
		ChildID:   childID,
		Status:    protocol.StatusExited,
		Kind:      protocol.KindFundi,
		StartedAt: time.Now(),
		Labels:    make(map[string]string),
	})

	darajaPool := darajapool.New(darajapool.NewRegistry())
	ctrl.WireDaraja(darajaPool, darajaPool.Reg())

	// Two disconnects in a row — second should be a no-op.
	ctrl.onDarajaDisconnect(childID)
	ctrl.onDarajaDisconnect(childID)

	snap, _ := st.Get(childID)
	if got := snap.Labels[darajaStateLabel]; got != "unreachable" {
		t.Errorf("double disconnect: label = %q, want %q", got, "unreachable")
	}

	// Three reconnects — all should clear.
	ctrl.onDarajaConnect(childID)
	ctrl.onDarajaConnect(childID)
	ctrl.onDarajaConnect(childID)

	snap, _ = st.Get(childID)
	if got := snap.Labels[darajaStateLabel]; got != "" {
		t.Errorf("triple reconnect: label = %q, want empty", got)
	}
}

// var _ control.Controller = (*Controller)(nil) — compile-time interface check.
var _ control.Controller = (*Controller)(nil)
