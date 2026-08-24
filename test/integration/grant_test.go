package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/executorsdb"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// grantDaemon boots the daemon with an executor listener wired to the test
// postgres, and returns it plus the executor store (for minting enrollment
// tokens) and the TLS cert fingerprint (for --pin-cert).
type grantDaemon struct {
	*daemon
	store       executors.Store
	fingerprint string
	listenAddr  string
}

// requireExecutorDB skips loudly unless the executor store can reach postgres.
func requireExecutorDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("RAFIKI_DB")
	}
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN (or RAFIKI_DB) is not set; executor grant end-to-end requires postgres")
	}
	return dsn
}

// grantCert writes a self-signed TLS cert + key to disk and returns the cert
// path, key path, and the leaf certificate's SHA-256 fingerprint (hex).
func grantCert(t *testing.T, dir string) (certPath, keyPath, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, "executor.crt")
	keyPath = filepath.Join(dir, "executor.key")
	if err := os.WriteFile(certPath, pemEncodeCert(der), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pemEncodeKey(keyDER), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	sum := sha256.Sum256(der)
	return certPath, keyPath, hex.EncodeToString(sum[:])
}

func pemEncodeCert(der []byte) []byte {
	return []byte("-----BEGIN CERTIFICATE-----\n" + chunk64(der) + "-----END CERTIFICATE-----\n")
}
func pemEncodeKey(der []byte) []byte {
	return []byte("-----BEGIN EC PRIVATE KEY-----\n" + chunk64(der) + "-----END EC PRIVATE KEY-----\n")
}

// chunk64 base64-wraps b at 64 columns, PEM-style.
func chunk64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for len(s) > 0 {
		n := 64
		if len(s) < n {
			n = len(s)
		}
		out.WriteString(s[:n])
		out.WriteByte('\n')
		s = s[n:]
	}
	return out.String()
}

// bootGrantDaemon starts rafikid with the executor listener wired to postgres.
// It reuses bootDaemon's env setup but additionally sets RAFIKI_DB, the
// executor listener, and the TLS cert/key.
func bootGrantDaemon(t *testing.T, dsn string) *grantDaemon {
	t.Helper()

	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	homeDir, err := os.MkdirTemp(base, "rafiki-grant")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	certPath, keyPath, fp := grantCert(t, homeDir)

	appDir := filepath.Join(homeDir, "rafiki")
	socketPath := filepath.Join(appDir, "controller.sock")
	if len(socketPath) > 100 {
		os.RemoveAll(homeDir)
		t.Fatalf("socket path too long: %s", socketPath)
	}

	// A free port for the executor listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(homeDir)
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"XDG_RUNTIME_DIR="+homeDir,
		"XDG_STATE_HOME="+homeDir,
		"XDG_DATA_HOME="+homeDir,
		"RAFIKI_PI_BINARY="+fakePiPath,
		"RAFIKI_DB="+dsn,
		"RAFIKI_EXECUTORS_ENABLED=1",
		"RAFIKI_CONTROL_LISTEN="+listenAddr,
		"RAFIKI_CONTROL_TLS_CERT="+certPath,
		"RAFIKI_CONTROL_TLS_KEY="+keyPath,
	)

	if err := cmd.Start(); err != nil {
		os.RemoveAll(homeDir)
		t.Fatalf("start daemon: %v", err)
	}

	d := &daemon{
		socketPath: socketPath,
		proc:       cmd,
		homeDir:    homeDir,
		logsDir:    filepath.Join(appDir, "logs"),
	}
	// Poll until the daemon accepts, same as bootDaemon.
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			_ = conn.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		d.stopDaemon()
		t.Fatalf("daemon never accepted on %s: %v", socketPath, lastErr)
	}
	t.Cleanup(d.stopDaemon)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		d.stopDaemon()
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// The constructor moved to pkg/executorsdb in 0acadf2 so pkg/executors
	// stays pgx-free; the Store INTERFACE is still in pkg/executors, which is
	// why grantDaemon.store keeps that type. Keeping the field typed as the
	// interface is the point of the split, not an accident.
	store := executorsdb.NewPostgresStore(pool)

	return &grantDaemon{daemon: d, store: store, fingerprint: fp, listenAddr: listenAddr}
}

// enrollExecutor mints a token and launches a native `rafiki executor serve`
// subprocess that reverse-dials the daemon with it. Native executors need no
// docker. Returns the enrolled executor's ID.
func (g *grantDaemon) enrollExecutor(t *testing.T, labels map[string]string) string {
	t.Helper()

	// A label unique to THIS enrollment, so the row can be identified by the
	// enrollment that created it rather than by the caller's labels.
	//
	// Matching on the caller's labels was wrong and silently so: the test
	// database accumulates executor rows across runs, every one of them left
	// with env=home and a non-zero LastSeenAt, so the loop below returned
	// whichever stale row came first. Tests that only inspected refusal MESSAGES
	// never noticed; the moment a test asserted on placement identity, it
	// compared against a row from a run an hour earlier.
	marker := fmt.Sprintf("t%d", time.Now().UnixNano())
	labels = maps.Clone(labels)
	labels["test-run"] = marker

	token, err := g.store.MintToken(context.Background(), executors.NewToken{
		Labels:        labels,
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	root := t.TempDir()
	credFile := filepath.Join(t.TempDir(), "cred")
	cmd := exec.Command(cliPath, "executor", "serve",
		"--connect", g.listenAddr,
		"--enroll-token", token,
		"--credential-file", credFile,
		"--pin-cert", g.fingerprint,
		"--root", root,
	)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start executor: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	// Wait for the executor to be enrolled and live: its row appears with
	// LastSeenAt set once the pool's Describe/Health handshake completes.
	deadline := time.Now().Add(10 * time.Second)
	var enrolledID string
	for time.Now().Before(deadline) {
		execs, _ := g.store.List(context.Background())
		for _, e := range execs {
			if e.Labels["test-run"] == marker && !e.LastSeenAt.IsZero() {
				enrolledID = e.ID
				break
			}
		}
		if enrolledID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if enrolledID == "" {
		t.Fatalf("executor %v never enrolled and became live", labels)
	}
	return enrolledID
}

// waitForLiveExecutors polls until the daemon's pool reports n live executors.
// The live set is only observable through selection, so this drives a
// throwaway spawn and inspects the refusal's live count until it reaches n.
func (g *grantDaemon) waitForLiveExecutors(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := g.grantSpawnRaw(t, "", "env=definitely-not-a-match", "anthropic/claude-x")
		msg := protocolErrorString(t, r)
		if strings.Contains(msg, fmt.Sprintf("%d live executor(s)", want)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("executors never became live (%d expected)", want)
}

// grantSpawnRaw sends ctrl_spawn with a parent and selector, returning the raw
// protocol.Response.
func (g *grantDaemon) grantSpawnRaw(t *testing.T, parent, selector, model string) protocol.Response {
	t.Helper()
	req := map[string]any{
		"type":             "ctrl_spawn",
		"id":               "grant",
		"cwd":              t.TempDir(),
		"kind":             "fundi",
		"model":            model,
		"noSession":        true,
		"parentChildId":    parent,
		"executorSelector": selector,
	}
	raw := g.request(t, mustMarshal(t, req))
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	return r
}

// TestGrant_NativeNarrowing drives the join over the wire with native
// executors (no docker): a coordinator confined to env=home spawns a worker
// naming env=work, and the refusal names the excluded executor.
func TestGrant_NativeNarrowing(t *testing.T) {
	dsn := requireExecutorDB(t)
	g := bootGrantDaemon(t, dsn)

	g.enrollExecutor(t, map[string]string{"env": "home"})
	g.enrollExecutor(t, map[string]string{"env": "work"})

	// Wait for both executors to be LIVE in the daemon's pool, not merely
	// enrolled. Enrollment's TouchSeen precedes the pool's live-map insert, so
	// a spawn racing it sees "0 live executors" and fails for the wrong reason.
	g.waitForLiveExecutors(t, 2)

	// The coordinator is a top-level agent (empty parent) that lands on env=home.
	coord := g.grantSpawnRaw(t, "", "env=home", "anthropic/claude-x")
	if !coord.Success {
		t.Fatalf("coordinator spawn failed: %+v", coord.Error)
	}
	var coordData protocol.SpawnResponseData
	mustUnmarshal(t, coord.Data, &coordData)
	coordID := coordData.ChildID

	// A worker under the coordinator naming env=work starts UNBOUND: a
	// parented child whose selector matches no live executor in its
	// effective set may wait for one to connect, because an executor
	// restart parks its connection for a full health tick and surviving
	// that window is what lazy binding exists for.
	unbound := g.grantSpawnRaw(t, coordID, "env=work", "anthropic/claude-x")
	if !unbound.Success {
		t.Fatalf("a parented worker with no matching executor must "+
			"start unbound, not be refused: %+v", unbound.Error)
	}
	var unboundData protocol.SpawnResponseData
	mustUnmarshal(t, unbound.Data, &unboundData)
	getRaw := g.request(t, mustMarshal(t, map[string]any{
		"type": "ctrl_get", "id": "g", "childId": unboundData.ChildID,
	}))
	var unboundGetR protocol.Response
	mustUnmarshal(t, getRaw, &unboundGetR)
	if !unboundGetR.Success {
		t.Fatalf("ctrl_get failed: %+v", unboundGetR.Error)
	}
	var unboundSummary protocol.ChildSummary
	mustUnmarshal(t, unboundGetR.Data, &unboundSummary)
	if unboundSummary.Labels["rafiki/executor-state"] != "unbound" {
		t.Fatalf("the worker outside its parent's set must carry "+
			"rafiki/executor-state=unbound, got labels=%v",
			unboundSummary.Labels)
	}

	// A worker naming env=home — inside the set — lands, and the child's
	// store record carries the executor it landed on (the same label phase 09
	// prompt visibility reads).
	ok := g.grantSpawnRaw(t, coordID, "env=home", "anthropic/claude-x")
	if !ok.Success {
		t.Fatalf("a worker inside the parent's set was refused: %+v", ok.Error)
	}
	var okData protocol.SpawnResponseData
	mustUnmarshal(t, ok.Data, &okData)
	okRaw := g.request(t, mustMarshal(t, map[string]any{
		"type": "ctrl_get", "id": "g", "childId": okData.ChildID,
	}))
	var okR protocol.Response
	mustUnmarshal(t, okRaw, &okR)
	if !okR.Success {
		t.Fatalf("ctrl_get failed: %+v", okR.Error)
	}
	var snap protocol.ChildSummary
	mustUnmarshal(t, okR.Data, &snap)
	landedID := snap.Labels["rafiki/executor"]
	if landedID == "" {
		t.Fatalf("worker did not record the executor it landed on (labels %v)", snap.Labels)
	}
	// The worker must have landed on an executor bearing the env=home label —
	// which, given multiple test runs, may be any such row, not one specific id.
	landed, err := g.store.Get(context.Background(), landedID)
	if err != nil {
		t.Fatalf("worker landed on unknown executor %q: %v", landedID, err)
	}
	if landed.Labels["env"] != "home" {
		t.Fatalf("worker landed on env=%q, want env=home (%v)", landed.Labels["env"], landed.Labels)
	}
}

func protocolErrorString(t *testing.T, r protocol.Response) string {
	t.Helper()
	if r.Error == nil {
		return ""
	}
	b, _ := json.Marshal(r.Error)
	return string(b)
}
