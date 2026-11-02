// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// This file recovers what the SDK's authorization-code handler discovered but
// does not return to its caller: the protected resource metadata (which names
// the canonical resource) and the authorization server metadata (which names
// the exact issuer).
//
// It does that without re-fetching anything and without inventing a second
// interpretation of the metadata. The SDK's fetcher needs the login URL before
// the SDK has finished validating anything, so the only truthful evidence
// available at that moment is what the SDK itself already fetched. A recorder
// under the flow's guarded client captures that traffic; an immutable replay
// client then feeds the EXACT recorded bytes back through the SDK's own public
// validators (oauthex.GetProtectedResourceMetadata, auth.GetAuthServerMetadata),
// so the validation and every fallback the SDK applies are the SDK's, not ours.
//
// Three rules follow from that, and they are why this is a recorder and not a
// parser:
//
//   - A response is never trusted for being JSON. A candidate only counts when
//     the SDK's validator accepts the recorded body for the resource the SDK
//     itself asked about, which is what the authorization URL's `resource`
//     parameter proves.
//   - The first arbitrary JSON body is never chosen. Candidates are replayed in
//     the SDK's own order, and the winner must agree with the resource the SDK
//     went on to use; a disagreement means this mirror and the SDK diverged, and
//     the flow fails closed instead of guessing.
//   - Mutable metadata is never fetched a second time to establish identity. The
//     replay transport serves recorded bytes only: a URL that was not recorded
//     is an error, not a request.

// discoveryMaxRecordedResponses bounds how many responses one login flow keeps.
// A flow makes a handful of metadata requests per consent round; the cap exists
// so a hostile or broken server cannot grow the recorder without bound.
const discoveryMaxRecordedResponses = 64

// errorReplayUnavailable reports that a replay could not be answered from
// recorded evidence: the request was never made, was made twice, or its body was
// not captured whole. It is always a reason to distrust a candidate, never a
// reason to go and ask again.
var errorReplayUnavailable = errors.New("mcpoauth: no recorded response")

// recordedResponse is one response the SDK consumed during a login round.
//
// body is filled as the SDK reads the body from the recorder, so it is a copy of
// exactly the bytes the SDK saw. Only a GET response gets a body recorded: that
// is what metadata discovery reads, and it keeps token responses (POST bodies)
// out of the recorder entirely.
type recordedResponse struct {
	method string
	url    string
	status int
	header http.Header

	mu       sync.Mutex
	body     bytes.Buffer
	bodyOK   bool
	bodySeen bool
	overflow bool
}

func (r *recordedResponse) key() string { return r.method + " " + r.url }

// complete reports whether this entry's body was captured whole.
func (r *recordedResponse) complete() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodySeen && r.bodyOK && !r.overflow
}

// usable reports whether this entry may answer a replay.
//
// A successful response must have been read to the end, because its body is what
// the validator judges. A failure response need not have been: the SDK's
// discovery helper closes a 4xx body without reading it, and its validator looks
// at the status alone — so replaying the status is replaying the evidence, not
// inventing a body.
func (r *recordedResponse) usable() bool {
	if r.status < 200 || r.status > 299 {
		return true
	}
	return r.complete()
}

// recordRequest is the RoundTripper under a login flow's client. It never
// changes a byte the SDK sees: the body is teed into the recorder as it is read.
type recordRequest struct {
	next http.RoundTripper
	// ctx is the login flow's context. Every request is issued under a context
	// that ends when either this or the SDK's own does, so abandoning a login
	// aborts the discovery request it was waiting on instead of letting it run
	// out its own timeout.
	ctx context.Context

	mu      sync.Mutex
	entries []*recordedResponse
}

func newRecordRequest(next http.RoundTripper, ctx context.Context) *recordRequest {
	return &recordRequest{next: next, ctx: ctx}
}

// wrap returns a copy of client whose transport records. The client itself is
// kept — its redirect rules and every destination check are the guarded
// client's, which is why this wraps a transport rather than building a client.
func (r *recordRequest) wrap(client *http.Client) *http.Client {
	out := *client
	out.Transport = r
	return &out
}

func (r *recordRequest) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, release := mergeContexts(req.Context(), r.ctx)
	resp, err := r.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		release()
		return nil, err
	}
	// The context stays alive until the body is closed: the policy client's
	// deadline covers the body read, and so does this.
	if resp.Body == nil {
		release()
	} else {
		resp.Body = &cancelOnCloseBody{rc: resp.Body, cancel: release}
	}
	entry := &recordedResponse{
		method: req.Method,
		url:    req.URL.String(),
		status: resp.StatusCode,
		header: resp.Header.Clone(),
	}
	r.mu.Lock()
	if len(r.entries) < discoveryMaxRecordedResponses {
		r.entries = append(r.entries, entry)
	}
	r.mu.Unlock()
	// Only a GET body is metadata. Teeing a POST response would put token
	// material in the recorder, and teeing the MCP endpoint's event stream would
	// buffer a stream that has no end.
	if req.Method == http.MethodGet && resp.Body != nil {
		resp.Body = &recordBody{rc: resp.Body, entry: entry}
	}
	return resp, nil
}

// mark returns the recorder's current position, which is the boundary a consent
// round starts from: everything recorded after it was fetched for that round.
func (r *recordRequest) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *recordRequest) since(mark int) []*recordedResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mark < 0 || mark > len(r.entries) {
		mark = 0
	}
	out := make([]*recordedResponse, len(r.entries)-mark)
	copy(out, r.entries[mark:])
	return out
}

// recordBody copies the bytes the SDK reads. Past the cap it stops recording and
// marks the entry unusable: a truncated body would replay as valid JSON garbage
// or, worse, as a *different* metadata document.
type recordBody struct {
	rc    io.ReadCloser
	entry *recordedResponse
}

func (b *recordBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		e := b.entry
		e.mu.Lock()
		if !e.overflow {
			if e.body.Len()+n > MaxResponseBytes {
				e.overflow = true
				e.body.Reset()
			} else {
				e.body.Write(p[:n])
			}
		}
		e.mu.Unlock()
	}
	if errors.Is(err, io.EOF) {
		e := b.entry
		e.mu.Lock()
		e.bodySeen = true
		e.bodyOK = !e.overflow
		e.mu.Unlock()
	}
	return n, err
}

func (b *recordBody) Close() error { return b.rc.Close() }

// replayTransport serves recorded responses back, immutably: one request per
// recorded response and nothing else. It is what makes "the exact bytes the SDK
// consumed" literal.
type replayTransport struct {
	entries []*recordedResponse

	mu     sync.Mutex
	served map[string]bool
}

func newReplayClient(p *Policy, entries []*recordedResponse) *http.Client {
	rt := &replayTransport{entries: entries, served: map[string]bool{}}
	c := &http.Client{Transport: rt}
	if p != nil {
		c.CheckRedirect = p.checkRedirect
	}
	return c
}

func (t *replayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.String()
	t.mu.Lock()
	if t.served[key] {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s was requested more than once", errorReplayUnavailable, key)
	}
	for _, e := range t.entries {
		if e.key() != key {
			continue
		}
		if !e.usable() {
			t.mu.Unlock()
			return nil, fmt.Errorf("%w: %s has no complete recorded body", errorReplayUnavailable, key)
		}
		t.served[key] = true
		t.mu.Unlock()
		return e.replay(req), nil
	}
	t.mu.Unlock()
	return nil, fmt.Errorf("%w: %s was never fetched", errorReplayUnavailable, key)
}

func (r *recordedResponse) replay(req *http.Request) *http.Response {
	r.mu.Lock()
	body := make([]byte, r.body.Len())
	copy(body, r.body.Bytes())
	r.mu.Unlock()
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		StatusCode:    r.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// prmCandidate is one protected-resource-metadata location the SDK tries, with
// the resource string it must declare to be accepted for that location
// (RFC 9728 section 3.3).
type prmCandidate struct {
	URL      string
	Resource string
}

// prmCandidates mirrors the SDK's protected-resource-metadata URL list exactly.
// It is written out here rather than imported because the SDK keeps it internal;
// if the SDK's order or spelling ever changes, the next rule below (the winner
// must agree with the SDK's own `resource` parameter) is what turns the
// divergence into a refused login instead of a wrong binding.
func prmCandidates(challengeMetadataURL, endpoint string) ([]prmCandidate, error) {
	var out []prmCandidate
	if challengeMetadataURL != "" {
		out = append(out, prmCandidate{URL: challengeMetadataURL, Resource: endpoint})
	}
	ru, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: mcp endpoint %q is not a URL: %w", endpoint, err)
	}
	mu := *ru
	// "At the path of the server's MCP endpoint".
	mu.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(ru.Path, "/")
	out = append(out, prmCandidate{URL: mu.String(), Resource: endpoint})
	// "At the root".
	mu.Path = "/.well-known/oauth-protected-resource"
	ru.Path = ""
	out = append(out, prmCandidate{URL: mu.String(), Resource: ru.String()})
	return out, nil
}

// validatedDiscovery is what a consent round established, already checked
// against the operator's destination policy.
type validatedDiscovery struct {
	// Resource is the canonical RFC 8707 resource key for the grant.
	Resource string
	// RawResource is the exact spelling the SDK used as the `resource`
	// parameter; it is the original string, not the canonical key.
	RawResource           string
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	// RevocationEndpoint is empty when the issuer advertised none.
	RevocationEndpoint string
}

// discoveryInput is everything one round's capture needs.
type discoveryInput struct {
	// Endpoint is the canonical MCP endpoint URL the flow dials.
	Endpoint string
	// AuthorizationURL is the URL the SDK produced for this round; its
	// `resource` parameter is the resource the SDK validated discovery for.
	AuthorizationURL string
	Recorder         *recordRequest
	// Mark is the recorder position this round started at.
	Mark   int
	Policy *Policy
}

// captureDiscovery identifies the metadata the SDK validated for one consent
// round from recorded evidence alone.
func captureDiscovery(ctx context.Context, in discoveryInput) (validatedDiscovery, error) {
	authURL, err := url.Parse(in.AuthorizationURL)
	if err != nil {
		return validatedDiscovery{}, fmt.Errorf("mcpoauth: authorization URL is not a URL: %w", err)
	}
	expected := authURL.Query().Get("resource")
	if expected == "" {
		return validatedDiscovery{}, errors.New("mcpoauth: the authorization request carries no resource parameter, so the resource it is for cannot be established")
	}
	entries := in.Recorder.since(in.Mark)

	challengeURL, err := recordedChallengeMetadataURL(entries, in.Endpoint)
	if err != nil {
		return validatedDiscovery{}, err
	}
	candidates, err := prmCandidates(challengeURL, in.Endpoint)
	if err != nil {
		return validatedDiscovery{}, err
	}

	prm, err := replayProtectedResourceMetadata(ctx, in.Policy, entries, candidates, expected, in.Endpoint)
	if err != nil {
		return validatedDiscovery{}, err
	}
	// The SDK reads AuthorizationServers[0] and nothing else, and its scheme
	// check lets an empty string through, so an empty first entry is not
	// "an issuer we did not check" — it is an issuer-less resource.
	if prm.AuthorizationServers[0] == "" {
		return validatedDiscovery{}, fmt.Errorf("mcpoauth: protected resource metadata for %s names an empty authorization server", prm.Resource)
	}

	asm, err := replayAuthServerMetadata(ctx, in.Policy, entries, prm.AuthorizationServers[0])
	if err != nil {
		return validatedDiscovery{}, err
	}

	out := validatedDiscovery{
		RawResource:           prm.Resource,
		Issuer:                asm.Issuer,
		AuthorizationEndpoint: asm.AuthorizationEndpoint,
		TokenEndpoint:         asm.TokenEndpoint,
		RevocationEndpoint:    asm.RevocationEndpoint,
	}
	if out.Resource, err = Canonicalize(prm.Resource); err != nil {
		return validatedDiscovery{}, fmt.Errorf("mcpoauth: discovered resource: %w", err)
	}
	if err := validateDiscovered(ctx, in.Policy, out); err != nil {
		return validatedDiscovery{}, err
	}
	return out, nil
}

// recordedChallengeMetadataURL finds the WWW-Authenticate resource_metadata
// parameter of the challenge that started this round. The SDK reads it from the
// 401/403 it was handed; the recorder saw the same response, so the same
// parameter is recovered here without asking again.
func recordedChallengeMetadataURL(entries []*recordedResponse, endpoint string) (string, error) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.status != http.StatusUnauthorized && e.status != http.StatusForbidden {
			continue
		}
		if canonical, err := Canonicalize(e.url); err != nil || canonical != endpoint {
			continue
		}
		challenges, err := oauthex.ParseWWWAuthenticate(e.header.Values("WWW-Authenticate"))
		if err != nil || len(challenges) == 0 {
			continue
		}
		for _, c := range challenges {
			if u := c.Params["resource_metadata"]; u != "" {
				return u, nil
			}
		}
		// A challenge without resource_metadata is legal; the SDK then goes
		// straight to the well-known locations.
		return "", nil
	}
	return "", fmt.Errorf("mcpoauth: no authentication challenge was recorded for %s in this consent round", endpoint)
}

// replayProtectedResourceMetadata re-runs the SDK's own PRM search over the
// recorded bytes, including its documented no-PRM fallback.
func replayProtectedResourceMetadata(ctx context.Context, p *Policy, entries []*recordedResponse, candidates []prmCandidate, expected, endpoint string) (*oauthex.ProtectedResourceMetadata, error) {
	for _, candidate := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, candidate.URL, candidate.Resource, newReplayClient(p, entries))
		if err != nil || prm == nil {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			// The SDK refuses this outright rather than searching further, and
			// so must the mirror: a PRM with no issuer cannot be one we skipped
			// past.
			return nil, fmt.Errorf("mcpoauth: protected resource metadata at %s has no authorization servers", candidate.URL)
		}
		return prm, nil
	}

	// The 2025-03-26 fallback: no protected resource metadata anywhere, so the
	// MCP server's root is taken as the authorization server and the endpoint as
	// the resource. It is reproduced only when the recorded attempts show that
	// EVERY candidate really was tried — otherwise "nothing validated" would be
	// indistinguishable from "the SDK never got that far".
	for _, candidate := range candidates {
		if !recordedGET(entries, candidate.URL) {
			return nil, fmt.Errorf("mcpoauth: no recorded protected resource metadata for %s and the fallback is not established", candidate.URL)
		}
	}
	// The fallback's own resource is the MCP endpoint itself. Anything else means
	// the SDK used a metadata document we could not reproduce, and a binding
	// invented from the fallback would claim a resource nobody validated.
	if expected != endpoint {
		return nil, fmt.Errorf("mcpoauth: the authorization request is for resource %q, which no validated metadata describes", expected)
	}
	ru, err := url.Parse(expected)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: discovered resource %q is not a URL: %w", expected, err)
	}
	root := *ru
	root.Path = ""
	return &oauthex.ProtectedResourceMetadata{
		AuthorizationServers: []string{root.String()},
		Resource:             expected,
	}, nil
}

// replayAuthServerMetadata re-runs the SDK's own AS-metadata search over the
// recorded bytes, including its predefined-endpoint fallback.
func replayAuthServerMetadata(ctx context.Context, p *Policy, entries []*recordedResponse, issuer string) (*oauthex.AuthServerMeta, error) {
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, newReplayClient(p, entries))
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: authorization server metadata for %s: %w", issuer, err)
	}
	if asm == nil {
		// The SDK's fallback for an issuer that publishes no metadata at all.
		// GetAuthServerMetadata returns nil only when every documented location
		// answered 4xx from the RECORDED set, so this is established, not guessed.
		return &oauthex.AuthServerMeta{
			Issuer:                issuer,
			AuthorizationEndpoint: issuer + "/authorize",
			TokenEndpoint:         issuer + "/token",
			RegistrationEndpoint:  issuer + "/register",
		}, nil
	}
	return asm, nil
}

// recordedGET reports whether a completed GET response for raw was recorded.
func recordedGET(entries []*recordedResponse, raw string) bool {
	for _, e := range entries {
		if e.method == http.MethodGet && e.url == raw && e.usable() {
			return true
		}
	}
	return false
}

// validateDiscovered applies what the SDK's validators do not: the operator's
// destination policy, to every URL the flow is about to use or hand to a
// browser. A resource, issuer or endpoint outside the policy refuses the login
// before any consent URL exists.
func validateDiscovered(ctx context.Context, p *Policy, d validatedDiscovery) error {
	if d.Issuer == "" {
		return errors.New("mcpoauth: discovered authorization server has no issuer")
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return fmt.Errorf("mcpoauth: authorization server %s advertises no authorization or token endpoint", d.Issuer)
	}
	targets := []struct {
		what string
		url  string
	}{
		{"resource", d.RawResource},
		{"issuer", d.Issuer},
		{"authorization endpoint", d.AuthorizationEndpoint},
		{"token endpoint", d.TokenEndpoint},
	}
	if d.RevocationEndpoint != "" {
		targets = append(targets, struct {
			what string
			url  string
		}{"revocation endpoint", d.RevocationEndpoint})
	}
	for _, t := range targets {
		if err := p.ValidateDestination(ctx, t.url); err != nil {
			return fmt.Errorf("mcpoauth: discovered %s %q: %w", t.what, t.url, err)
		}
	}
	return nil
}
