// SPDX-License-Identifier: Apache-2.0

// Package darajapool holds the daemon's live daraja connections and the
// credentials that admit them.
//
// Deliberately NOT execpool.Pool. That type carries database rows, health
// polling, park windows, workspace provisioning and admission selectors, none
// of which apply here: a daraja has no row, is one-to-one with a child the
// daemon already knows, and is replaced rather than repaired.
package darajapool

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"
)

// Registry mints one-shot launch tickets and holds per-child reconnect
// credentials.
//
// Everything is in memory and nothing is persisted, matching
// execpool.TicketRegistry's reasoning: a ticket is meaningful only while the
// daemon that minted it is running, and a reconnect credential names a process
// that dies with this daemon's ability to reach it. A restart therefore refuses
// every reconnect — see DarajaHelloResponse.Retryable for why that is intended.
type Registry struct {
	mu      sync.Mutex
	tickets map[string]string // ticket -> childID
	creds   map[string]string // childID -> credential
}

func NewRegistry() *Registry {
	return &Registry{tickets: map[string]string{}, creds: map[string]string{}}
}

func newSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("darajapool: generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func (r *Registry) MintTicket(childID string) (string, error) {
	tk, err := newSecret()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.tickets[tk] = childID
	r.mu.Unlock()
	return tk, nil
}

// RedeemTicket consumes a ticket. One-shot: replaying one must not authenticate
// a second daraja onto the same child.
func (r *Registry) RedeemTicket(ticket string) (string, bool) {
	if ticket == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.tickets[ticket]
	if ok {
		delete(r.tickets, ticket)
	}
	return id, ok
}

func (r *Registry) RevokeTicket(ticket string) {
	r.mu.Lock()
	delete(r.tickets, ticket)
	r.mu.Unlock()
}

// IssueCredential replaces any credential this child had, so the newest
// connection is the only one that can come back.
func (r *Registry) IssueCredential(childID string) (string, error) {
	cred, err := newSecret()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.creds[childID] = cred
	r.mu.Unlock()
	return cred, nil
}

// CheckCredential reports whether cred is childID's current credential.
//
// Constant-time compare: this is a bearer secret presented by an unauthenticated
// peer, and the tickets/credentials here are the only thing standing between a
// local process and another child's conversation.
func (r *Registry) CheckCredential(cred, childID string) bool {
	if cred == "" {
		return false
	}
	r.mu.Lock()
	want := r.creds[childID]
	r.mu.Unlock()
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cred), []byte(want)) == 1
}

// Forget ends a child's ability to connect at all. Called when the child is
// killed or closed.
func (r *Registry) Forget(childID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.creds, childID)
	for tk, id := range r.tickets {
		if id == childID {
			delete(r.tickets, tk)
		}
	}
}
