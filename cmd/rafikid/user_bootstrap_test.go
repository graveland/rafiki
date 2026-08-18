package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// bootstrapStore is a users.Store whose active count and Create behaviour the
// test drives directly.
type bootstrapStore struct {
	active     int
	countErr   error
	created    []string
	createErr  error
	countCalls int
}

func (b *bootstrapStore) CountActive(context.Context) (int, error) {
	b.countCalls++
	return b.active, b.countErr
}

func (b *bootstrapStore) Create(_ context.Context, username string) (users.User, string, error) {
	if b.createErr != nil {
		return users.User{}, "", b.createErr
	}
	b.created = append(b.created, username)
	b.active++
	return users.User{ID: "u" + username, Username: username}, "rfk_tok", nil
}

func (b *bootstrapStore) Authenticate(context.Context, string) (users.Identity, error) {
	panic("unused")
}
func (b *bootstrapStore) List(context.Context, bool, int) ([]users.User, error) { panic("unused") }
func (b *bootstrapStore) Delete(context.Context, string) error                  { panic("unused") }

// The re-check belongs with the store, not with the connection: admission was
// decided once, at accept time, and a peer that holds that connection open
// must not keep minting users after the first one closed the window.
func TestUserCreateBootstrapRefusesOnceAUserExists(t *testing.T) {
	st := &bootstrapStore{}
	c := &Controller{users: st}

	if _, err := c.UserCreateBootstrap(context.Background(), "first"); err != nil {
		t.Fatalf("the first bootstrap create failed: %v", err)
	}

	_, err := c.UserCreateBootstrap(context.Background(), "mallory")
	if err == nil {
		t.Fatal("a second bootstrap create succeeded after the window closed")
	}
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrAuthRequired {
		t.Fatalf("err = %v, want a ControllerError with code %q", err, protocol.ErrAuthRequired)
	}
	if len(st.created) != 1 {
		t.Fatalf("created = %v, want exactly one user", st.created)
	}
}

// Every call re-reads the store. A cached or once-only check is precisely the
// defect this method exists to close.
func TestUserCreateBootstrapChecksTheStoreOnEveryCall(t *testing.T) {
	st := &bootstrapStore{}
	c := &Controller{users: st}
	for i := 0; i < 3; i++ {
		_, _ = c.UserCreateBootstrap(context.Background(), "u")
	}
	if st.countCalls != 3 {
		t.Fatalf("CountActive called %d times, want 3 (one per request)", st.countCalls)
	}
}

// "I could not check" is neither an admission nor a closed window: the create
// must not run, and the store's error must propagate as an error rather than
// as an auth answer. (The dispatcher strips its text — see
// TestADispatchErrorNeverLeaksTheStoresTextToABootstrapPeer.)
func TestUserCreateBootstrapRefusesWhenTheCountCannotBeRead(t *testing.T) {
	st := &bootstrapStore{countErr: errors.New("failed to connect to host=db.internal")}
	c := &Controller{users: st}

	_, err := c.UserCreateBootstrap(context.Background(), "brent")
	if err == nil {
		t.Fatal("bootstrap create succeeded with an unreadable user count")
	}
	var ce *control.ControllerError
	if errors.As(err, &ce) {
		t.Fatalf("a store outage became a protocol answer: %+v", ce)
	}
	if len(st.created) != 0 {
		t.Fatalf("created = %v, want none", st.created)
	}
	if !strings.Contains(err.Error(), "db.internal") {
		t.Fatalf("the store error was swallowed: %v", err)
	}
}

// A daemon with no database cannot serve identity at all.
func TestUserCreateBootstrapNeedsAStore(t *testing.T) {
	c := &Controller{}
	_, err := c.UserCreateBootstrap(context.Background(), "brent")
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrNoAgentDB {
		t.Fatalf("err = %v, want a ControllerError with code %q", err, protocol.ErrNoAgentDB)
	}
}
