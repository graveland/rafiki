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
// A flow makes a handful of metadata requests per consent round, so reaching this
// is a server asking for far more than any login needs. It is a hard stop, not a
// quiet one: see recordRequest.overflowAt.
const discoveryMaxRecordedResponses = 64

// errObservationOverflow reports that a login flow made more HTTP requests than
// the recorder keeps, so the evidence for its consent round no longer exists.
//
// It is deliberately its own error: the alternative is the mirror looking at an
// empty window and refusing the round with "no authentication challenge", which
// blames the wrong thing and hides a recording limit behind a server's fault.
var errObservationOverflow = errors.New("mcpoauth: the login made more requests than can be recorded")

// errorReplayUnavailable reports that a replay could not be answered from
// recorded evidence: the request was never made, or its body was not captured
// whole. It is always a reason to distrust a candidate, never a reason to go and
// ask again.
var errorReplayUnavailable = errors.New("mcpoauth: no recorded response")

// errorReplayAmbiguous reports that one request was recorded more than once in
// the same consent round, with no way to tell which response the SDK was handed.
//
// This is the one observation failure that is fatal to a whole round rather than
// to one candidate. A duplicate means the same URL answered differently twice
// inside a single round — a hostile server's cheapest way to make a recorder see
// one body and the SDK see another. Replaying "the first one" would then be
// replaying a response the SDK may never have read, and the difference would end
// up in the persisted binding. Nothing about the round can be trusted after that,
// so the login is refused with an error that says why instead of falling back to
// a resource nobody validated.
var errorReplayAmbiguous = errors.New("mcpoauth: a request was recorded more than once")

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

// contentType is the media type this response was served with.
func (r *recordedResponse) contentType() string {
	return r.header.Get("Content-Type")
}

// markComplete records that this entry's body was read to its end.
func (r *recordedResponse) markComplete() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodySeen = true
	r.bodyOK = !r.overflow
}

// complete reports whether this entry's body was captured whole.
func (r *recordedResponse) complete() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodySeen && r.bodyOK && !r.overflow
}

// equivalentTo reports whether another recording of the same request carries the
// same answer: same status, same body, same media type.
//
// Identical duplicates are not ambiguous — replaying either one replays what the
// SDK saw, because both are the same bytes. It is the SDK asking the same URL
// twice and getting the SAME answer, which is ordinary for a server that answers
// a challenge-URL candidate and a well-known candidate with one document.
func (r *recordedResponse) equivalentTo(other *recordedResponse) bool {
	if other == nil || r.status != other.status {
		return false
	}
	if r.header.Get("Content-Type") != other.header.Get("Content-Type") {
		return false
	}
	r.mu.Lock()
	mine := r.body.Bytes()
	r.mu.Unlock()
	other.mu.Lock()
	theirs := other.body.Bytes()
	other.mu.Unlock()
	return bytes.Equal(mine, theirs)
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
	// overflowAt is the entry count at which the cap was first reached, or 0 when
	// it never was. Once set, nothing further is recorded and no body is teed: an
	// entry kept outside the slice would be an orphan whose body grew with no
	// reader, and a window that silently lost entries is not evidence.
	overflowAt int
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
	recorded := len(r.entries) < discoveryMaxRecordedResponses
	if recorded {
		r.entries = append(r.entries, entry)
	} else if r.overflowAt == 0 {
		r.overflowAt = len(r.entries)
	}
	r.mu.Unlock()
	// Only a GET body is metadata, and only an entry this recorder keeps gets one.
	// Teeing a POST response would put token material in the recorder, teeing the
	// MCP endpoint's event stream would buffer a stream that has no end, and teeing
	// into a dropped entry would be memory nobody can read.
	if recorded && req.Method == http.MethodGet && resp.Body != nil {
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

// since returns the evidence recorded after mark, or errObservationOverflow when
// the recorder stopped keeping entries at or after that position.
func (r *recordRequest) since(mark int) ([]*recordedResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mark < 0 || mark > len(r.entries) {
		mark = 0
	}
	if r.overflowAt > 0 && mark <= r.overflowAt {
		return nil, fmt.Errorf("%w: the cap is %d responses", errObservationOverflow, discoveryMaxRecordedResponses)
	}
	out := make([]*recordedResponse, len(r.entries)-mark)
	copy(out, r.entries[mark:])
	return out, nil
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
		b.record(p[:n])
	}
	if errors.Is(err, io.EOF) {
		b.entry.markComplete()
	}
	return n, err
}

// record appends bytes the SDK read. Past the cap the entry is marked unusable and
// the buffer dropped: a truncated body would replay as valid JSON garbage or, worse,
// as a *different* metadata document.
func (b *recordBody) record(p []byte) {
	e := b.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.overflow {
		return
	}
	if e.body.Len()+len(p) > MaxResponseBytes {
		e.overflow = true
		e.body.Reset()
		return
	}
	e.body.Write(p)
}

// Close drains whatever the SDK left unread, then closes.
//
// It is not tidiness, it is what makes the recording complete for a whole class of
// servers. The SDK decodes metadata with json.Decoder.Decode, which stops at the end
// of the first JSON value: on a CHUNKED response — a handler that flushes, a body
// built as it is written, most CDN and proxy paths — Decode returns after one value
// and the body never reaches EOF. Without this drain every such document would be
// incomplete evidence, every candidate would be unusable, the no-metadata fallback
// could not be established, and a perfectly good server would be refused at login.
//
// The recording then holds a SUPERSET of the bytes the SDK read, which is still
// exact evidence: json.Decoder.Decode yields the same first value from a longer
// buffer, so the validator judges precisely the document the SDK judged. Past the
// cap the entry becomes the explicit too-large case, never a truncation.
func (b *recordBody) Close() error {
	if b.drain() {
		b.entry.markComplete()
	}
	return b.rc.Close()
}

// drain reads the rest of the body into the recording and reports whether it
// reached EOF. It stops as soon as the entry overflows: the remaining bytes cannot
// make the evidence usable again, and reading them would only hold the connection.
//
// It does not run at all for an event stream. That body has no end by design, so
// draining it would block Close for as long as the stream lives — and the exchange
// this protects is metadata, which is never an event stream. It is unreachable
// today because the login transport disables the standalone SSE stream, which is
// precisely why the rule belongs here rather than in a comment: the day that flag
// changes, Close must still return. isEventStream is the network layer's own
// content-type rule, reused so there is one spelling of it.
func (b *recordBody) drain() bool {
	if isEventStream(b.entry.contentType()) {
		return false
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := b.rc.Read(buf)
		if n > 0 {
			b.record(buf[:n])
			b.entry.mu.Lock()
			overflow := b.entry.overflow
			b.entry.mu.Unlock()
			if overflow {
				return false
			}
		}
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

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
	// The key is the exact request; the label is what an error may name. A
	// metadata URL can carry a query (a resource with one propagates into its
	// candidates), and an error string ends up in a log.
	key := req.Method + " " + req.URL.String()
	label := req.Method + " " + redactTarget(req.URL)
	t.mu.Lock()
	if t.served[key] {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s was requested more than once", errorReplayUnavailable, label)
	}
	var match *recordedResponse
	duplicates := 0
	for _, e := range t.entries {
		if e.key() != key {
			continue
		}
		duplicates++
		if match == nil {
			match = e
		}
	}
	// Two DIFFERENT recorded answers for one request are ambiguous evidence: the
	// SDK may have been handed either of them, so replaying the first would be
	// replaying a body the SDK might not have consumed. Refusing is the only safe
	// answer — picking one silently is how a mirror binds a resource the SDK never
	// saw. Identical duplicates are fine: either one IS what the SDK read.
	if duplicates > 1 {
		for _, e := range t.entries {
			if e.key() != key || e == match {
				continue
			}
			if !match.equivalentTo(e) {
				t.mu.Unlock()
				return nil, fmt.Errorf("%w: %s was fetched %d times with different answers, so which response the SDK read is unknown", errorReplayAmbiguous, label, duplicates)
			}
		}
	}
	if match == nil {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s was never fetched", errorReplayUnavailable, label)
	}
	if !match.usable() {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s has no complete recorded body", errorReplayUnavailable, label)
	}
	t.served[key] = true
	t.mu.Unlock()
	return match.replay(req), nil
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
		// Authored, and the parse error is not wrapped: url.Parse quotes the URL it
		// was given, and this one may carry userinfo and a query token.
		return nil, fmt.Errorf("mcpoauth: mcp endpoint %q is not a usable URL: %w", redactRawTarget(endpoint), ErrInvalidURL)
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
	// RegistrationEndpoint is what dynamic client registration was sent to. It is
	// validated like every other advertised endpoint, and the check is
	// deliberately post-hoc: DCR runs BEFORE the fetcher that captures this
	// metadata, so by the time the endpoint is named here the registration has
	// already happened — under the guarded client, whose dial-time check refused an
	// out-of-policy destination then. Validating it here still fails the login
	// closed when discovery names a registration endpoint the operator's policy
	// would not allow, instead of accepting a round whose metadata contradicts the
	// policy in a way the next step might trust.
	RegistrationEndpoint string
	// TokenEndpointAuthMethods is the issuer's
	// token_endpoint_auth_methods_supported, which is what decides how a client
	// secret is presented once a login is over: the login persists the resolved
	// style, so a refresh never has to guess and never has to retry.
	TokenEndpointAuthMethods []string
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
		// Authored, never wrapped: url.Parse's error quotes the URL it was given,
		// and this one carries the PKCE challenge and the state.
		return validatedDiscovery{}, fmt.Errorf("mcpoauth: the authorization URL the SDK produced is not a URL: %w", ErrInvalidURL)
	}
	expected := authURL.Query().Get("resource")
	if expected == "" {
		return validatedDiscovery{}, errors.New("mcpoauth: the authorization request carries no resource parameter, so the resource it is for cannot be established")
	}
	entries, err := in.Recorder.since(in.Mark)
	if err != nil {
		return validatedDiscovery{}, err
	}

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
	// The cross-check that makes this a mirror and not a second opinion: whatever
	// this replay accepted must be the resource the SDK itself went on to request
	// authorization for. The comparison is RAW — the SDK passes the metadata's own
	// spelling as `resource`, so any difference means the mirror and the SDK picked
	// different metadata, and the flow must not bind anything. Canonicalisation
	// happens only below, after this passes.
	if prm.Resource != expected {
		return validatedDiscovery{}, fmt.Errorf("mcpoauth: the protected resource metadata this login would bind is for %q, but the authorization request is for %q", redactRawTarget(prm.Resource), redactRawTarget(expected))
	}
	// The SDK reads AuthorizationServers[0] and nothing else, and its scheme
	// check lets an empty string through, so an empty first entry is not
	// "an issuer we did not check" — it is an issuer-less resource.
	if prm.AuthorizationServers[0] == "" {
		return validatedDiscovery{}, fmt.Errorf("mcpoauth: protected resource metadata for %s names an empty authorization server", redactRawTarget(prm.Resource))
	}

	asm, err := replayAuthServerMetadata(ctx, in.Policy, entries, prm.AuthorizationServers[0])
	if err != nil {
		return validatedDiscovery{}, err
	}

	out := validatedDiscovery{
		RawResource:              prm.Resource,
		Issuer:                   asm.Issuer,
		AuthorizationEndpoint:    asm.AuthorizationEndpoint,
		TokenEndpoint:            asm.TokenEndpoint,
		RevocationEndpoint:       asm.RevocationEndpoint,
		RegistrationEndpoint:     asm.RegistrationEndpoint,
		TokenEndpointAuthMethods: asm.TokenEndpointAuthMethodsSupported,
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
	// The LAST challenge to the endpoint in the window, scanned backwards.
	//
	// Every earlier 401/403 in a round was answered with a retry, not with a consent
	// round: a 403 whose challenge is not insufficient_scope makes the SDK return
	// nil and re-send the request, and the retry's 401 is the response whose
	// metadata actually drove discovery. Taking the first one would build the
	// candidates from a document the SDK never fetched. A step-up round has exactly
	// one challenge, so this costs nothing there.
	var chosen *recordedResponse
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.status != http.StatusUnauthorized && e.status != http.StatusForbidden {
			continue
		}
		canonical, err := Canonicalize(e.url)
		if err != nil || canonical != endpoint {
			continue
		}
		// The candidate URLs are derived from the CANONICAL endpoint, while the SDK
		// derives its own from req.URL.String(). They agree because this engine
		// dials the canonical form; if a future change dials a different spelling,
		// every login would otherwise fail closed with "no authentication
		// challenge", blaming the server for our own mismatch. Say what happened.
		if e.url != endpoint {
			return "", fmt.Errorf("mcpoauth: the transport dialled %s but this login derives its metadata URLs from %s",
				redactRawTarget(e.url), redactRawTarget(endpoint))
		}
		chosen = e
		break
	}
	if chosen == nil {
		return "", fmt.Errorf("mcpoauth: no authentication challenge was recorded for %s in this consent round", redactRawTarget(endpoint))
	}

	challenges, err := oauthex.ParseWWWAuthenticate(chosen.header.Values("WWW-Authenticate"))
	if err != nil {
		// The SDK fails Authorize outright when the challenge it was handed does not
		// parse, so there is nothing here to skip past: the round would be over.
		return "", fmt.Errorf("mcpoauth: the authentication challenge for %s cannot be parsed", redactRawTarget(endpoint))
	}
	if len(challenges) == 0 {
		// No challenge at all (an empty header) is not a parse failure: the SDK then
		// goes straight to the well-known locations.
		return "", nil
	}
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			return u, nil
		}
	}
	// A challenge without resource_metadata is legal; the SDK then goes straight to
	// the well-known locations.
	return "", nil
}

// replayProtectedResourceMetadata re-runs the SDK's own PRM search over the
// recorded bytes, including its documented no-PRM fallback.
func replayProtectedResourceMetadata(ctx context.Context, p *Policy, entries []*recordedResponse, candidates []prmCandidate, expected, endpoint string) (*oauthex.ProtectedResourceMetadata, error) {
	for _, candidate := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, candidate.URL, candidate.Resource, newReplayClient(p, entries))
		if err != nil {
			// Ambiguity is not a reason to try the next candidate: it means this
			// round's evidence cannot be reproduced at all, and the fallback below
			// would be built on the same ambiguity.
			if errors.Is(err, errorReplayAmbiguous) {
				// Authored, and the inner error is NOT wrapped: both the SDK's own
				// message and the replay transport's quote the request URL, which here
				// carries the resource's query. Only the sentinel survives, so a caller
				// can still tell what kind of failure this is.
				return nil, fmt.Errorf("mcpoauth: the metadata for %s cannot be replayed, because the same request was answered twice: %w",
					redactRawTarget(candidate.URL), errorReplayAmbiguous)
			}
			continue
		}
		if prm == nil {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			// The SDK refuses this outright rather than searching further, and
			// so must the mirror: a PRM with no issuer cannot be one we skipped
			// past.
			return nil, fmt.Errorf("mcpoauth: protected resource metadata at %s has no authorization servers", redactRawTarget(candidate.URL))
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
			return nil, fmt.Errorf("mcpoauth: no recorded protected resource metadata for %s and the fallback is not established", redactRawTarget(candidate.URL))
		}
	}
	// The fallback's own resource is the MCP endpoint itself. Anything else means
	// the SDK used a metadata document we could not reproduce, and a binding
	// invented from the fallback would claim a resource nobody validated.
	if expected != endpoint {
		return nil, fmt.Errorf("mcpoauth: the authorization request is for resource %q, which no validated metadata describes", redactRawTarget(expected))
	}
	ru, err := url.Parse(expected)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: discovered resource %q is not a URL: %w", redactRawTarget(expected), err)
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
		// Authored, never wrapped: the SDK's own metadata errors quote whatever the
		// server sent, and an error message is not a place to repeat it. The issuer
		// is named because it is public, and the reason is not.
		return nil, fmt.Errorf("mcpoauth: the authorization server metadata for %s is not usable", redactRawTarget(issuer))
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
		return fmt.Errorf("mcpoauth: authorization server %s advertises no authorization or token endpoint", redactRawTarget(d.Issuer))
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
	if d.RegistrationEndpoint != "" {
		targets = append(targets, struct {
			what string
			url  string
		}{"registration endpoint", d.RegistrationEndpoint})
	}
	for _, t := range targets {
		if err := p.ValidateDestination(ctx, t.url); err != nil {
			return fmt.Errorf("mcpoauth: discovered %s %q: %w", t.what, redactRawTarget(t.url), err)
		}
	}
	return nil
}
