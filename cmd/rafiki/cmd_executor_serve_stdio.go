// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/version"
)

// `executor serve --stdio` is the far end of an ssh install: a launcher starts
// this process over ssh, the daemon writes a one-shot ticket as the FIRST LINE
// of stdin, and the ordinary executor protocol then speaks on stdin/stdout.
//
// Two invariants make it work:
//
//   - the ticket is never in argv. argv is visible in `ps` on the target, and a
//     ticket is a credential for as long as it is unused.
//   - stdout carries ONLY the protocol. Anything else — a log line, a banner —
//     is read as a frame by the daemon's HTTP/2 client and kills the session,
//     so the default logger is pinned to stderr before anything can log.

// stdioTicketMax bounds the ticket line. The daemon mints short opaque tickets;
// a longer "line" is a protocol mistake, not a credential.
const stdioTicketMax = 256

// stdioInstallIDRE mirrors the executor RPC's install_id rule. The value is
// written as the marker's first line, so it must be one word.
var stdioInstallIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// stdioServeConfig is the slice of `executor serve`'s flags the --stdio path
// uses. The connect, credential and relay flags are deliberately absent: a
// --stdio executor has no daemon address and no durable credential of its own.
type stdioServeConfig struct {
	Root            string
	Concurrency     int
	RTKMode         string
	SpillDir        string
	JobOutputBudget int64
	LSPConfig       string
	NoLSP           bool
	InstallID       string
}

// runStdioServeCommand is the whole --stdio path: validate the flags, build the
// ordinary executor server, and serve it on stdin/stdout.
func runStdioServeCommand(cmd *cobra.Command, cfg stdioServeConfig) error {
	// Before ANYTHING can log, including the env-file loader below.
	setupStdioLogging(os.Stderr)

	if err := validateStdioFlags(cmd, cfg.InstallID); err != nil {
		return err
	}
	wd, err := resolveRoot(cfg.Root)
	if err != nil {
		return err
	}
	srv := executor.NewServer(executor.Options{
		Root:            wd,
		Concurrency:     cfg.Concurrency,
		Version:         version.String(),
		RTK:             tools.ParseRTKMode(cfg.RTKMode),
		SpillDir:        cfg.SpillDir,
		JobOutputBudget: cfg.JobOutputBudget,
		LSPConfig:       cfg.LSPConfig,
		NoLSP:           cfg.NoLSP,
		// Skills sync defaults OFF in --stdio mode whatever the flags, the
		// environment or --launch say. This process is a TARGET a launcher
		// installed, and rewriting the machine's Claude skills directory is not
		// part of being installed; the operator can still run an ordinary
		// `executor serve` if they want the corpus pushed here.
		SkillsSync:    false,
		PyModulesSync: false,
		RepoSync:      false,
		Env:           executorPinnedEnv(),
	})
	defer func() { _ = srv.Close() }()

	return runStdioServe(cmdCtx(cmd), os.Stdin, os.Stdout, cfg.InstallID, executorHandler(srv, nil))
}

// validateStdioFlags refuses the combinations that cannot work. The transport
// flags are refused by NAME: an operator whose unit file still carries
// --connect needs to be told which flag conflicts, not handed a process that
// silently ignores it.
func validateStdioFlags(cmd *cobra.Command, installID string) error {
	for _, name := range []string{"connect", "connect-socket", "credential", "credential-file", "enroll-token"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			continue
		}
		if cmd.Flags().Changed(name) || f.Value.String() != "" {
			return fmt.Errorf("--stdio cannot be combined with --%s: a --stdio executor's only credential "+
				"is the one-shot ticket on stdin, and its only connection is stdin/stdout", name)
		}
	}
	if installID == "" {
		return errors.New("--stdio requires --install-id")
	}
	if !stdioInstallIDRE.MatchString(installID) {
		return fmt.Errorf("--install-id %q must be 1-64 characters of letters, digits, '-' or '_'", installID)
	}
	return nil
}

// runStdioServe reads the ticket, wraps stdin/stdout as a connection, and runs
// the ordinary executor upgrade over it. It returns when the daemon's session
// ends (or the process is cancelled).
func runStdioServe(ctx context.Context, in io.Reader, out io.Writer, installID string, handler http.Handler) error {
	setupStdioLogging(os.Stderr)

	ticket, err := readStdioTicket(in)
	if err != nil {
		return fmt.Errorf("--stdio: reading the ticket from stdin: %w", err)
	}
	if ticket == "" {
		return errors.New("--stdio: no ticket on stdin; the launcher writes it as the first line")
	}

	conn := &stdioConn{in: in, out: out}
	// The marker is written from the wrapped handler, so it is only written
	// once a request has actually been served — i.e. once the upgrade
	// succeeded. A process that never connects leaves the target looking empty,
	// which is the truth.
	wrapped := &installMarkerHandler{next: handler, installID: installID}
	if err := execpool.Connect(ctx, stdioConnectOptions(conn, ticket, wrapped)); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// stdioConnectOptions is the ConnectOptions a --stdio executor uses: the
// injected connection and the ticket, nothing dialled and nothing written to
// disk.
func stdioConnectOptions(conn net.Conn, ticket string, handler http.Handler) execpool.ConnectOptions {
	return execpool.ConnectOptions{
		Conn:    conn,
		Ticket:  ticket,
		Handler: handler,
		SelfReported: map[string]string{
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"version": version.String(),
		},
	}
}

// readStdioTicket reads ONE line, at most stdioTicketMax bytes, from r.
//
// It reads a byte at a time on purpose. The bytes AFTER the newline are the
// start of the upgrade request, and a bufio.Reader would consume them into a
// buffer this function throws away — the connection would then begin mid
// request. One syscall per byte for a <=256-byte line is nothing.
func readStdioTicket(r io.Reader) (string, error) {
	var line []byte
	one := make([]byte, 1)
	for len(line) <= stdioTicketMax {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSpace(string(line)), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return strings.TrimSpace(string(line)), nil
			}
			return "", err
		}
	}
	return "", fmt.Errorf("the ticket line on stdin is longer than %d bytes", stdioTicketMax)
}

// setupStdioLogging pins the default logger to w — os.Stderr in production.
// stdout is the protocol channel, so no log line may ever reach it.
func setupStdioLogging(w io.Writer) {
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
}

// installMarkerHandler writes the target's install marker the first time a
// request is served. A request can only arrive after the upgrade succeeded,
// which is exactly the moment the install is known to be live.
type installMarkerHandler struct {
	next      http.Handler
	installID string
	once      sync.Once
}

func (h *installMarkerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() {
		if err := writeInstallMarker(h.installID); err != nil {
			// Not fatal: the marker is the clobber guard, not the protocol. The
			// next install's probe will see an empty target, which is the same
			// answer a fresh install gets.
			slog.Error("executor: could not record the install marker",
				"installId", h.installID, "error", err)
		}
	})
	h.next.ServeHTTP(w, r)
}

// writeInstallMarker writes <data dir>/installed-executor atomically: a temp
// file in the same directory, chmod 0600, then rename.
func writeInstallMarker(installID string) error {
	dir := paths.DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".installed-executor-*")
	if err != nil {
		return fmt.Errorf("create a marker temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after a successful rename
	if _, err := f.WriteString(installID + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "installed-executor")); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// stdioConn is stdin/stdout as a net.Conn: Read from stdin, Write to stdout.
// Close closes stdout first, then stdin — the order the remote process's own
// shutdown uses, so a peer reading our last frame sees EOF after it.
type stdioConn struct {
	in   io.Reader
	out  io.Writer
	once sync.Once
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.out.Write(p) }

func (c *stdioConn) Close() error {
	var err error
	c.once.Do(func() {
		if cl, ok := c.out.(io.Closer); ok {
			err = cl.Close()
		}
		if cl, ok := c.in.(io.Closer); ok {
			if cerr := cl.Close(); err == nil {
				err = cerr
			}
		}
	})
	return err
}

func (c *stdioConn) LocalAddr() net.Addr  { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr { return stdioAddr{} }

// Deadlines are no-ops: a pipe has none, and the upgrade's fixed 10s bound is
// enforced by the caller's context instead.
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }
