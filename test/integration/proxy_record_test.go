// SPDX-License-Identifier: Apache-2.0

package integration_test

// The standalone proxy recorder, driven as the built binary rather than as a
// library: `rafikid proxy record` is the one rafikid command path that runs
// with no daemon, no database and no profile, and only a subprocess can prove
// that from the outside. The unit tests in cmd/rafikid call the handler
// directly, so they cannot say anything about what the binary loads, binds or
// leaves on disk — which is the whole point of the command.
//
// The recorder is handed an environment built from scratch (recorderEnv), with
// no RAFIKI_DB, no RAFIKI_TEST_DSN, no RAFIKI_DAEMON_ID and no
// XDG_CONFIG_HOME. Once it has served three exchanges the entire temp HOME must
// still be empty, so a control socket, a logs directory, a state tree or a
// profile cache would each fail this test rather than hide behind a clean exit
// code.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

const (
	// The recorder's announce lines. The binary also emits one unrelated
	// pre-cobra "INFO loaded environment file …" line when a service.env
	// exists, so every wait here matches a prefix rather than "the first
	// stderr line".
	proxyRecordListenPrefix = "rafikid proxy record: listening on "
	proxyRecordDirPrefix    = "rafikid proxy record: recording to "
	proxyRecordWarnPrefix   = "rafikid proxy record: WARNING: "

	// proxyRecordFixedDate pins the fake upstream's Date header, which net/http
	// would otherwise stamp with the wall clock, so a recorded response file can
	// be compared byte for byte.
	proxyRecordFixedDate = "Mon, 01 Jan 2024 00:00:00 GMT"

	// The fake upstream picks its answer from the request's model, the way the
	// real endpoint picks its shape from `stream` and its refusals from the
	// account behind the credential.
	proxyRecordModelJSON = "claude-proxy-record-json"
	proxyRecordModelSSE  = "claude-proxy-record-sse"
	proxyRecordModel429  = "claude-proxy-record-429"
)

// The fake upstream's three answers. Each is a constant so the recorded
// response file can be written out in full and compared byte for byte.
const (
	proxyRecordJSONBody = `{"id":"msg_01","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"hello"}]}`
	proxyRecord429Body = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
)

// proxyRecordSSEEvents is a multi-event stream with pings between the deltas,
// the shape a real streaming /v1/messages answer takes. Joined, it is the exact
// response body.
var proxyRecordSSEEvents = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_02\"}}\n\n",
	"event: ping\ndata: {\"type\":\"ping\"}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0," +
		"\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n",
	"event: ping\ndata: {\"type\":\"ping\"}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0," +
		"\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// TestProxyRecordStandaloneBinary runs the built binary as `proxy record` in
// front of a fake Anthropic-compatible upstream and proves, from outside the
// process, that it is isolated and that its recordings are byte-exact.
func TestProxyRecordStandaloneBinary(t *testing.T) {
	c := assert.NewAborting(t)

	// daemonBinary, never binaryPath: the helper opens every build input so the
	// go test cache is keyed to the sources this subprocess was built from.
	bin := daemonBinary()

	upstream := newProxyRecordUpstream(t)
	env := newRecorderEnv(t)
	dir := filepath.Join(t.TempDir(), "recordings")

	rec := startProxyRecordProcess(t, bin, env, dir, upstream.URL)
	c.Eq(dir, rec.dir, "the recorder announces the resolved record directory")
	c.StrContains(rec.stderr.String(), proxyRecordWarnPrefix,
		"the secrets warning must be printed on every start")

	// ── exchange 1: a successful JSON answer ────────────────────────────────
	jsonRequestBody := `{"model":"` + proxyRecordModelJSON +
		`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	jsonRaw := rawProxyRecordRequest(rec.host, jsonRequestBody)
	json := rec.exchange(t, 1, jsonRaw)

	c.Eq(http.StatusOK, json.status, "the client must see the upstream status")
	c.EqDeep([]byte(proxyRecordJSONBody), json.body, "the client must receive the upstream body unchanged")

	c.EqDeep([]byte(jsonRaw), json.request.raw,
		"the recorded request must be the bytes that arrived, header for header")
	method, target := json.request.requestLine(t)
	c.Eq("POST", method, "recorded request method")
	c.Eq("/v1/messages?beta=1", target, "the recorded request keeps the path and query as received")
	c.EqDeep([]byte(jsonRequestBody), json.request.body, "the recorded request body must be byte-exact")
	c.Eq(rec.host, proxyRecordHeaderValue(t, json.request, "Host"),
		"the recorded request carries the client's Host, which net/http lifts out of the header map")
	c.Eq("Bearer sk-ant-oat01-proxy-record", proxyRecordHeaderValue(t, json.request, "Authorization"),
		"credentials are recorded verbatim: this directory is forensic input")

	wantJSONResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Length: " + strconv.Itoa(len(proxyRecordJSONBody)) + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Date: " + proxyRecordFixedDate + "\r\n" +
		"Request-Id: req-json\r\n" +
		"\r\n" + proxyRecordJSONBody
	c.EqDeep([]byte(wantJSONResponse), json.response.raw,
		"the recorded response must be the bytes the client received")
	c.Eq(http.StatusOK, json.response.statusCode(t), "recorded status")
	c.EqDeep([]byte(proxyRecordJSONBody), json.response.body, "the recorded response body must be byte-exact")
	c.Eq("application/json", proxyRecordHeaderValue(t, json.response, "Content-Type"), "recorded content type")

	// ── exchange 2: a multi-event SSE stream with pings ─────────────────────
	sseRequestBody := `{"model":"` + proxyRecordModelSSE + `","stream":true,"max_tokens":16}`
	sse := rec.exchange(t, 2, rawProxyRecordRequest(rec.host, sseRequestBody))
	sseStream := strings.Join(proxyRecordSSEEvents, "")

	c.Eq(http.StatusOK, sse.status, "the client must see the upstream status")
	c.EqDeep([]byte(sseStream), sse.body, "the client must receive every SSE event, framing included")
	c.EqDeep([]byte(sseRequestBody), sse.request.body, "the recorded request body must be byte-exact")

	// No Content-Length and no Transfer-Encoding: the recorded body is the
	// de-chunked stream, exactly as it crossed the proxy boundary, so a
	// replayer re-frames rather than trusting a recorded length.
	wantSSEResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/event-stream\r\n" +
		"Date: " + proxyRecordFixedDate + "\r\n" +
		"Request-Id: req-sse\r\n" +
		"\r\n" + sseStream
	c.EqDeep([]byte(wantSSEResponse), sse.response.raw,
		"the recorded stream must be the bytes relayed, event framing and pings included")
	c.Eq("text/event-stream", proxyRecordHeaderValue(t, sse.response, "Content-Type"), "recorded content type")
	_, declaredLength := sse.response.header("Content-Length")
	c.False(declaredLength, "a flushed stream declares no Content-Length")

	// ── exchange 3: the provider's 429 ─────────────────────────────────────
	rateLimitedBody := `{"model":"` + proxyRecordModel429 + `","max_tokens":4}`
	rateLimited := rec.exchange(t, 3, rawProxyRecordRequest(rec.host, rateLimitedBody))

	c.Eq(http.StatusTooManyRequests, rateLimited.status, "a provider refusal is relayed, not translated")
	c.EqDeep([]byte(proxyRecord429Body), rateLimited.body, "the client must receive the provider's error body")

	want429Response := "HTTP/1.1 429 Too Many Requests\r\n" +
		"Content-Length: " + strconv.Itoa(len(proxyRecord429Body)) + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Date: " + proxyRecordFixedDate + "\r\n" +
		"Request-Id: req-429\r\n" +
		"Retry-After: 7\r\n" +
		"\r\n" + proxyRecord429Body
	c.EqDeep([]byte(want429Response), rateLimited.response.raw, "the 429 is recorded as it came back")
	c.Eq(http.StatusTooManyRequests, rateLimited.response.statusCode(t), "recorded status")
	c.EqDeep([]byte(proxyRecord429Body), rateLimited.response.body, "the recorded 429 body must be byte-exact")

	// The recorder is a recorder, not a replayer: the upstream must have seen
	// the same bytes the files hold.
	forwarded := upstream.requests()
	c.Len(forwarded, 3, "every accepted exchange must reach the upstream")
	c.Eq("POST /v1/messages?beta=1", forwarded[0].method+" "+forwarded[0].target,
		"the recorder forwards the path and query it received")
	c.EqDeep([]byte(jsonRequestBody), forwarded[0].body, "the recorder forwards the request body unchanged")
	c.Eq("Bearer sk-ant-oat01-proxy-record", forwarded[0].header.Get("Authorization"),
		"the credential must reach the upstream verbatim")
	c.Eq("sk-ant-api03-proxy-record", forwarded[0].header.Get("X-Api-Key"),
		"the api key must reach the upstream verbatim")

	// ── exactly three complete pairs, private, in arrival order ────────────
	wantNames := []string{
		"000001-request.http", "000001-response.http",
		"000002-request.http", "000002-response.http",
		"000003-request.http", "000003-response.http",
	}
	c.EqDeep(wantNames, recordDirNames(t, dir),
		"the record directory must hold exactly three pairs and no staging leftovers")
	for _, name := range wantNames {
		info, err := os.Stat(filepath.Join(dir, name))
		c.NoError(err, "stat %s", name)
		c.Eq(os.FileMode(0o600), info.Mode().Perm(), "%s must be 0600", name)
	}
	dirInfo, err := os.Stat(dir)
	c.NoError(err, "stat the record directory")
	c.Eq(os.FileMode(0o700), dirInfo.Mode().Perm(), "the record directory must be 0700")

	// ── only POST /v1/messages is served ──────────────────────────────────
	for _, raw := range []string{
		"POST /v1/other HTTP/1.1\r\nHost: " + rec.host + "\r\nContent-Length: 2\r\n\r\n{}",
		"GET /v1/messages HTTP/1.1\r\nHost: " + rec.host + "\r\n\r\n",
	} {
		status, _, _ := postRawToRecorder(t, rec.host, raw)
		c.Eq(http.StatusNotFound, status, "only POST /v1/messages is served, got %q",
			strings.SplitN(raw, "\r\n", 2)[0])
	}
	c.EqDeep(wantNames, recordDirNames(t, dir), "a rejected route must add no pair")
	c.Eq(3, upstream.count(), "a rejected route must not reach the upstream")

	// ── isolation: nothing a daemon owns may appear ────────────────────────
	// The recorder could not have opened a database even if it wanted one — its
	// environment names no DSN — and the fact that it started and served is the
	// proof it did not need one. Everything a daemon would leave behind lives
	// inside env.home, because RuntimeDir, StateDir and DataDir are all
	// $XDG_*/rafiki under it and ConfigDir falls back to $HOME/.config/rafiki.
	// All of it must still be empty.
	c.Empty(recordTreeNames(t, env.home), "a standalone recorder must leave no daemon state tree behind")
	c.NoFileExists(filepath.Join(env.home, "rafiki", "controller.sock"), "no control socket")
	c.NoFileExists(filepath.Join(env.home, "rafiki", "executor.sock"), "no executor socket")
	c.NoDirExists(filepath.Join(env.home, ".config", "rafiki"), "no config tree")
	c.Empty(recordTreeNames(t, env.tmp),
		"the recorder must stage its temp files in the record directory, not in TMPDIR")

	// ── a second instance refuses a nonempty directory, before binding ─────
	// It is handed the FIRST recorder's port: were the directory check to run
	// after net.Listen, this instance would fail with "address already in use"
	// instead, so the message it exits with is itself the proof of the order.
	_, port, err := net.SplitHostPort(rec.host)
	c.NoError(err, "split the announced listener address %q", rec.host)
	second := exec.Command(bin, "proxy", "record",
		"--listen", "127.0.0.1:"+port,
		"--upstream", upstream.URL,
		"--record-dir", dir)
	second.Env = env.environ()
	secondStderr := &stderrBuf{}
	second.Stdout = &stderrBuf{}
	second.Stderr = secondStderr
	c.Error(second.Run(), "a recorder pointed at a nonempty directory must exit non-zero")
	c.StrContains(secondStderr.String(), "invalid --record-dir", "the refusal must name the flag")
	c.StrContains(secondStderr.String(), "is not empty", "the refusal must say why")
	c.NotStrContains(secondStderr.String(), proxyRecordListenPrefix, "the refusal must happen before binding")
	c.NotStrContains(secondStderr.String(), "address already in use",
		"the directory check must run before net.Listen")
	c.EqDeep(wantNames, recordDirNames(t, dir), "a refused start must leave the recordings untouched")

	// ── SIGTERM drains and exits clean ─────────────────────────────────────
	c.NoError(rec.stop(t), "the recorder must exit 0 on SIGTERM; stderr:\n%s", rec.stderr.tail(4000))
	c.Empty(rec.stdout.String(), "the announcements belong on stderr")
	c.EqDeep(wantNames, recordDirNames(t, dir), "every pair must still be on disk after shutdown")
}

// ─── the recorder subprocess ───────────────────────────────────────────────

// recorderEnv is the whole environment a recorder subprocess gets: a temp HOME
// with every XDG base pointed inside it, and nothing else from the shell.
//
// Building it from scratch rather than filtering os.Environ() IS the isolation
// claim. The developer's shell exports RAFIKI_DB and RAFIKI_TEST_DSN (the
// suite's .env), and a recorder that inherited them would prove nothing about
// running without a database. XDG_CONFIG_HOME is deliberately absent as well —
// ConfigDir then falls back to $HOME/.config, inside the tree this test checks
// for leftovers — and no RAFIKI_DAEMON_ID is set, so the process has no daemon
// identity at all.
type recorderEnv struct {
	home string
	tmp  string
}

func newRecorderEnv(t *testing.T) recorderEnv {
	t.Helper()
	return recorderEnv{home: t.TempDir(), tmp: t.TempDir()}
}

func (e recorderEnv) environ() []string {
	return []string{
		"HOME=" + e.home,
		"TMPDIR=" + e.tmp,
		"XDG_RUNTIME_DIR=" + e.home,
		"XDG_STATE_HOME=" + e.home,
		"XDG_DATA_HOME=" + e.home,
	}
}

// standaloneRecorder is one `rafikid proxy record` subprocess, as announced.
type standaloneRecorder struct {
	cmd     *exec.Cmd
	exited  chan struct{} // closed once cmd.Wait returns
	exitErr error         // valid only after exited is closed
	stderr  *stderrBuf
	stdout  *stderrBuf
	// host and dir come from the announce lines: the address actually bound
	// (so --listen 127.0.0.1:0 is usable), and the resolved absolute record
	// directory.
	host string
	dir  string

	stopOnce sync.Once
}

func startProxyRecordProcess(t *testing.T, bin string, env recorderEnv, dir, upstream string) *standaloneRecorder {
	t.Helper()
	cmd := exec.Command(bin, "proxy", "record",
		"--listen", "127.0.0.1:0",
		"--upstream", upstream,
		"--record-dir", dir)
	cmd.Env = env.environ()
	rec := &standaloneRecorder{
		cmd:    cmd,
		exited: make(chan struct{}),
		stderr: &stderrBuf{},
		stdout: &stderrBuf{},
	}
	cmd.Stderr = rec.stderr
	cmd.Stdout = rec.stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rafikid proxy record: %v", err)
	}
	go func() {
		rec.exitErr = cmd.Wait()
		close(rec.exited)
	}()
	t.Cleanup(func() { _ = rec.stop(t) })
	rec.waitAnnouncement(t)
	return rec
}

// waitAnnouncement waits for the recorder's startup block on stderr and records
// the address it actually bound. A bounded poll on the announced line, never a
// sleep: the line is printed after net.Listen succeeds and before Serve, so a
// client connecting once it is seen is queued by the listen backlog.
func (r *standaloneRecorder) waitAnnouncement(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		stderr := r.stderr.String()
		url, haveURL := announcedAfter(stderr, proxyRecordListenPrefix)
		dir, haveDir := announcedAfter(stderr, proxyRecordDirPrefix)
		if haveURL && haveDir && strings.Contains(stderr, proxyRecordWarnPrefix) {
			r.dir = dir
			r.host = strings.TrimPrefix(url, "http://")
			return
		}
		select {
		case <-r.exited:
			t.Fatalf("the recorder exited before announcing a listener (exit error: %v)\nstderr:\n%s",
				r.exitErr, r.stderr.tail(4000))
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the recorder never announced its listener\nstderr:\n%s", r.stderr.tail(4000))
		}
	}
}

// announcedAfter returns the trimmed remainder of the first line that follows
// prefix.
func announcedAfter(s, prefix string) (string, bool) {
	i := strings.Index(s, prefix)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(prefix):]
	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:end]), true
}

// stop terminates the recorder with SIGTERM — the signal the service units use
// — and returns its exit error. It is idempotent, so the test's explicit stop
// and the cleanup can both call it.
func (r *standaloneRecorder) stop(t *testing.T) error {
	t.Helper()
	r.stopOnce.Do(func() {
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-r.exited:
		case <-time.After(30 * time.Second):
			_ = r.cmd.Process.Kill()
			<-r.exited
			t.Errorf("the recorder did not exit within 30s of SIGTERM\nstderr:\n%s", r.stderr.tail(4000))
		}
	})
	return r.exitErr
}

// ─── one exchange ──────────────────────────────────────────────────────────

// proxyRecordExchange is one round trip through the recorder: what the client
// saw, and the pair that landed on disk.
type proxyRecordExchange struct {
	status   int
	body     []byte
	request  proxyRecordFile
	response proxyRecordFile
}

// exchange sends raw over a fresh connection and then WAITS for the response
// file. The recorder commits the pair only after the client has its last byte,
// so the file may not exist the moment the response is read; the response half
// is renamed second, so its presence implies a complete pair.
func (r *standaloneRecorder) exchange(t *testing.T, seq int, raw string) proxyRecordExchange {
	t.Helper()
	c := assert.NewAborting(t)
	status, _, body := postRawToRecorder(t, r.host, raw)

	responseName := fmt.Sprintf("%06d-response.http", seq)
	c.EventuallyTrue(10*time.Second, 5*time.Millisecond, func() bool {
		_, err := os.Stat(filepath.Join(r.dir, responseName))
		return err == nil
	}, "%s was never recorded", responseName)

	return proxyRecordExchange{
		status:   status,
		body:     body,
		request:  readProxyRecordFile(t, filepath.Join(r.dir, fmt.Sprintf("%06d-request.http", seq))),
		response: readProxyRecordFile(t, filepath.Join(r.dir, responseName)),
	}
}

// rawProxyRecordRequest is the exact request a client sends for one exchange.
// The header lines are already in the byte order net/http writes them
// (Header.Write sorts keys), so the expected request file is this string,
// unchanged — which is what makes the comparison byte for byte.
func rawProxyRecordRequest(host, body string) string {
	return "POST /v1/messages?beta=1 HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Authorization: Bearer sk-ant-oat01-proxy-record\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Content-Type: application/json\r\n" +
		"X-Api-Key: sk-ant-api03-proxy-record\r\n" +
		"\r\n" + body
}

// postRawToRecorder speaks HTTP/1.1 by hand, so the recorded request holds
// exactly the bytes the test wrote and nothing a Go client would have added on
// its own.
func postRawToRecorder(t *testing.T, host, raw string) (int, http.Header, []byte) {
	t.Helper()
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial the recorder at %s: %v", host, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the recorder's response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the recorder's response body: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// ─── the .http format ──────────────────────────────────────────────────────

// proxyRecordFile is one recorded .http file: the start line, the headers in
// file order, and the body exactly as it sits after the first blank line. raw
// keeps the file as written, so a whole file can be compared byte for byte.
type proxyRecordFile struct {
	file      string
	startLine string
	headers   []proxyRecordHeader
	body      []byte
	raw       []byte
}

type proxyRecordHeader struct{ name, value string }

func readProxyRecordFile(t *testing.T, path string) proxyRecordFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return parseProxyRecordFile(t, filepath.Base(path), raw)
}

// parseProxyRecordFile parses the recorder's format: start line, MIME-style
// headers, a CRLF blank line, then the body. It is hand-written rather than
// textproto.ReadMIMEHeader because it has to prove the CRLF framing and hand
// back the body verbatim; a header map expresses neither.
func parseProxyRecordFile(t *testing.T, name string, raw []byte) proxyRecordFile {
	t.Helper()
	const separator = "\r\n\r\n"
	split := bytes.Index(raw, []byte(separator))
	if split < 0 {
		t.Fatalf("%s: no CRLF blank line between the head and the body; the file must use CRLF line endings", name)
	}
	lines := strings.Split(string(raw[:split]), "\r\n")
	for _, line := range lines {
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("%s: head line %q holds a bare CR or LF; every line must end with CRLF", name, line)
		}
	}
	parsed := proxyRecordFile{
		file:      name,
		startLine: lines[0],
		body:      raw[split+len(separator):],
		raw:       raw,
	}
	for _, line := range lines[1:] {
		headerName, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("%s: header line %q is not \"Name: value\"", name, line)
		}
		parsed.headers = append(parsed.headers, proxyRecordHeader{name: headerName, value: value})
	}
	return parsed
}

// header returns the first value recorded under name, compared
// case-insensitively because Go canonicalises header keys on the way in.
func (f proxyRecordFile) header(name string) (string, bool) {
	for _, h := range f.headers {
		if strings.EqualFold(h.name, name) {
			return h.value, true
		}
	}
	return "", false
}

// proxyRecordHeaderValue fails the test unless name is present.
func proxyRecordHeaderValue(t *testing.T, f proxyRecordFile, name string) string {
	t.Helper()
	value, ok := f.header(name)
	if !ok {
		t.Fatalf("%s: no %s header; headers were %v", f.file, name, f.headers)
	}
	return value
}

// requestLine splits a request start line: "POST /v1/messages?beta=1 HTTP/1.1".
func (f proxyRecordFile) requestLine(t *testing.T) (method, target string) {
	t.Helper()
	fields := strings.Fields(f.startLine)
	if len(fields) != 3 || fields[2] != "HTTP/1.1" {
		t.Fatalf("%s: start line %q is not \"METHOD TARGET HTTP/1.1\"", f.file, f.startLine)
	}
	return fields[0], fields[1]
}

// statusCode parses a response start line: "HTTP/1.1 429 Too Many Requests".
func (f proxyRecordFile) statusCode(t *testing.T) int {
	t.Helper()
	fields := strings.SplitN(f.startLine, " ", 3)
	if len(fields) < 2 || fields[0] != "HTTP/1.1" {
		t.Fatalf("%s: start line %q is not \"HTTP/1.1 CODE REASON\"", f.file, f.startLine)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("%s: start line %q has no numeric status code", f.file, f.startLine)
	}
	return code
}

// ─── the directory and the fake upstream ───────────────────────────────────

// recordDirNames lists the record directory's entries. os.ReadDir already sorts
// by name, and the lexical order of %06d names is arrival order.
func recordDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the record directory %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// recordTreeNames returns every path under root, relative to it. A tree that
// must not exist at all shows up here as its entries.
func recordTreeNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return names
}

// proxyRecordUpstream is a fake Anthropic-compatible /v1/messages endpoint: it
// answers in the shape the request's model asks for and keeps every request it
// received, so the test can prove the recorder forwarded the bytes rather than
// only recording them.
type proxyRecordUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	received []proxyRecordForwarded
}

type proxyRecordForwarded struct {
	method string
	target string
	header http.Header
	body   []byte
}

func newProxyRecordUpstream(t *testing.T) *proxyRecordUpstream {
	t.Helper()
	upstream := &proxyRecordUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *proxyRecordUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.received)
}

func (u *proxyRecordUpstream) requests() []proxyRecordForwarded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]proxyRecordForwarded(nil), u.received...)
}

func (u *proxyRecordUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.received = append(u.received, proxyRecordForwarded{
		method: r.Method,
		target: r.URL.RequestURI(),
		header: r.Header.Clone(),
		body:   body,
	})
	u.mu.Unlock()

	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.Error(w, "proxy record upstream: unexpected route", http.StatusNotFound)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)

	switch req.Model {
	case proxyRecordModelJSON:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", proxyRecordFixedDate)
		w.Header().Set("Request-Id", "req-json")
		_, _ = io.WriteString(w, proxyRecordJSONBody)
	case proxyRecordModelSSE:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Date", proxyRecordFixedDate)
		w.Header().Set("Request-Id", "req-sse")
		w.WriteHeader(http.StatusOK)
		for _, event := range proxyRecordSSEEvents {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
	case proxyRecordModel429:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", proxyRecordFixedDate)
		w.Header().Set("Request-Id", "req-429")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, proxyRecord429Body)
	default:
		http.Error(w, "proxy record upstream: unknown model "+req.Model, http.StatusBadRequest)
	}
}
