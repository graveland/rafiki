// SPDX-License-Identifier: Apache-2.0

package execpool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executor"
)

// proxyPoolFixture stands up a real pkg/executor.Server (with the given
// proxy allowlist) behind an in-process inverted connection, joins it to a
// fresh Pool, and returns the pool and the executor ID once the executor is
// live. The mapping under test is proxyTransport's; the transport underneath
// it is the real thing, exercised end to end.
func proxyPoolFixture(t *testing.T, proxies map[string]string) (*Pool, string) {
	t.Helper()
	const executorID = "exec-proxy"

	store := newFakeStore(executorID)
	p := New(store)
	p.healthInterval = time.Hour // no health polling noise in this test

	srv := executor.NewServer(executor.Options{Root: t.TempDir(), NoLSP: true, Proxies: proxies})
	go p.handleConn(invertedPair(t, srv))

	waitFor(t, 5*time.Second, "executor to join", func() bool { return len(p.Live()) == 1 })
	return p, executorID
}

// The transport turns one http.Request into a ProxyStart plus body chunks, and
// one ProxyHead plus body chunks back into an http.Response.
func TestProxyTransportRoundTrip(t *testing.T) {
	var gotPath, gotHeader, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotHeader = r.Header.Get("X-Test")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Reply", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("world"))
	}))
	defer upstream.Close()

	p, executorID := proxyPoolFixture(t, map[string]string{"vmlx": upstream.URL})

	rt := NewProxyTransport(p, executorID, "vmlx")
	req, err := http.NewRequest(http.MethodPost, "http://ignored.invalid/v1/messages?x=1",
		strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test", "yes")

	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if got := resp.Header.Get("X-Reply"); got != "yes" {
		t.Errorf("response header X-Reply = %q, want yes", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "world" {
		t.Errorf("body = %q, want %q", body, "world")
	}

	if gotPath != "/v1/messages?x=1" {
		t.Errorf("upstream saw path %q, want %q", gotPath, "/v1/messages?x=1")
	}
	if gotHeader != "yes" {
		t.Errorf("upstream saw X-Test = %q, want yes", gotHeader)
	}
	if gotBody != "hello" {
		t.Errorf("upstream saw body %q, want %q", gotBody, "hello")
	}
}

// A response must be readable before the upstream finishes — an SSE stream
// that only arrives at EOF is useless for a streaming turn.
func TestProxyTransportStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("httptest ResponseWriter is not a Flusher")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a"))
		flusher.Flush()
		<-release
		_, _ = w.Write([]byte("b"))
	}))
	defer upstream.Close()

	p, executorID := proxyPoolFixture(t, map[string]string{"vmlx": upstream.URL})
	rt := NewProxyTransport(p, executorID, "vmlx")

	resp, err := (&http.Client{Transport: rt}).Get("http://ignored.invalid/stream")
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, 1)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, first)
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read first byte: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive the first chunk before the upstream finished writing")
	}
	if string(first) != "a" {
		t.Fatalf("first byte = %q, want %q", first, "a")
	}

	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if string(rest) != "b" {
		t.Fatalf("rest = %q, want %q", rest, "b")
	}
}

func TestProxyTransportUndeclaredNameSurfacesError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("undeclared proxy name must never reach the upstream")
	}))
	defer upstream.Close()

	p, executorID := proxyPoolFixture(t, map[string]string{"vmlx": upstream.URL})
	rt := NewProxyTransport(p, executorID, "nope")

	resp, err := (&http.Client{Transport: rt}).Get("http://ignored.invalid/v1/messages")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("RoundTrip succeeded with status %d, want an error naming the undeclared proxy", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to mention the proxy name %q", err.Error(), "nope")
	}
}

// A caller resolving connectClientFor for an executor that never connected
// must get a plain, immediate error rather than a nil-pointer panic reaching
// into Proxy.
func TestProxyTransportUnknownExecutor(t *testing.T) {
	p := New(newFakeStore("exec-other"))
	rt := NewProxyTransport(p, "exec-does-not-exist", "vmlx")

	_, err := (&http.Client{Transport: rt}).Get("http://ignored.invalid/v1/messages")
	if err == nil {
		t.Fatal("RoundTrip succeeded against an executor that was never connected")
	}
}

// A background goroutine mutating req.Header while RoundTrip reads it would be
// a caller bug, not this code's — but the transport's OWN concurrent halves
// (the send loop and the body reader) must not race each other. -race is what
// actually enforces this; the assertions just confirm the mapping still comes
// out right under it.
func TestProxyTransportConcurrentRoundTripsDoNotRace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, r.Body)
	}))
	defer upstream.Close()

	p, executorID := proxyPoolFixture(t, map[string]string{"vmlx": upstream.URL})
	rt := NewProxyTransport(p, executorID, "vmlx")
	client := &http.Client{Transport: rt}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Post("http://ignored.invalid/echo", "text/plain", strings.NewReader("ping"))
			if err != nil {
				t.Errorf("RoundTrip: %v", err)
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
				return
			}
			if string(b) != "ping" {
				t.Errorf("body = %q, want %q", b, "ping")
			}
		}()
	}
	wg.Wait()
}
