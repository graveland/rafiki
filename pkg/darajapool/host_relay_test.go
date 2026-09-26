// SPDX-License-Identifier: Apache-2.0

package darajapool

// The END-TO-END daraja leg with a REAL host and a REAL server: a script
// binary runs under a daraja.Host, the daraja.Server relays its events over
// the pool's hijacked connection, and the daemon-side Runner must deliver
// stdout AND the exit — the two things a script child's settle is computed
// from. This is the seam the wave-4 script hosting depends on: an exit lost
// between the host and the runner leaves the child "streaming" forever, which
// is exactly the failure mode this test exists to catch.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/daraja"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

func testScriptBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-script")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRealHostScriptExitReachesTheRunner(t *testing.T) {
	// The sleep keeps the host's events from landing before the pool's relay
	// stream attaches, separating an attach race from a broken relay.
	bin := testScriptBinary(t, `sleep 0.5; echo relayed-out; echo relayed-err >&2; exit 3`)
	host := daraja.NewHost(daraja.HostOptions{
		Binary: bin,
		Spec:   daraja.ChildSpec{Kind: daraja.KindScript, ExtraArgs: []string{bin}},
	})
	if err := host.Start(); err != nil {
		t.Fatalf("host start: %v", err)
	}

	pool, _, teardown := connectFakeDaraja(t, daraja.NewServer(host))
	defer teardown()

	// The harness returns before handleConn finishes installing the live
	// connection; a Watch before that fails and the pump's 500 ms retry then
	// races the script's own output (a broadcast to zero subscribers is
	// dropped). Wait for the connection to be live, the way Launch's OnConnect
	// wait orders production.
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := false
		for _, id := range pool.Live() {
			if id == "c1" {
				live = true
			}
		}
		if live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daraja connection never became live in the pool")
		}
		time.Sleep(10 * time.Millisecond)
	}

	r := NewRunner(pool, "c1")
	_, stdoutR, stderrR, err := r.Start()
	if err != nil {
		t.Fatalf("runner start: %v", err)
	}

	// Wait is the settle input: it must return the script's exit code.
	done := make(chan [2]string, 1)
	go func() {
		code, sig := r.Wait()
		done <- [2]string{itoa(code), sig}
	}()

	// Both pipes are synchronous, and the stdout/stderr pumps are separate
	// goroutines feeding ONE drain loop: a consumer that stops reading either
	// pipe stalls the other. pkg/child drains both to EOF on their own
	// goroutines — this test does the same (read to EOF, not to a byte count:
	// a partial read leaves the pipe's write un-consumed and the pump stuck).
	type result struct {
		name string
		data string
		err  error
	}
	results := make(chan result, 2)
	readAll := func(name string, rd io.Reader) {
		b, err := readAllTimeout(rd, 15*time.Second)
		results <- result{name: name, data: string(b), err: err}
	}
	go readAll("stderr", stderrR)
	go readAll("stdout", stdoutR)
	for range 2 {
		select {
		case res := <-results:
			if res.err != nil {
				t.Fatalf("read %s: %v", res.name, res.err)
			}
			want := "relayed-out"
			if res.name == "stderr" {
				want = "relayed-err"
			}
			if !strings.Contains(res.data, want) {
				t.Fatalf("%s = %q, want it to contain %q", res.name, res.data, want)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("timeout reading the relayed streams")
		}
	}

	select {
	case got := <-done:
		if got[0] != "3" || got[1] != "" {
			t.Fatalf("Wait = %v, want code 3 with no signal", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the script's exit never reached the runner (the child would hang as streaming forever)")
	}
}

// itoa keeps the test free of strconv for two digits.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// readAllTimeout is io.ReadAll with a deadline, so a lost relay stalls the
// test's failure path instead of hanging it.
func readAllTimeout(r io.Reader, d time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		ch <- result{b, err}
	}()
	select {
	case res := <-ch:
		return res.b, res.err
	case <-time.After(d):
		return nil, errReadTimeout
	}
}

type readTimeoutError struct{}

func (readTimeoutError) Error() string { return "read timeout" }

var errReadTimeout error = readTimeoutError{}

// Compile-time assertion that the fake spec type is still the wire type the
// host consumes (a drifted darajapb would break the launch payload silently).
var _ = darajapb.Kind_KIND_SCRIPT
