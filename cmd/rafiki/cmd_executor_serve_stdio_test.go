// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/version"

	"github.com/multigres/testkit/assert"
)

func TestServeStdioReadsTicketFromStdinNotArgv(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"newline", "ticket-1\nrest-of-the-stream", "ticket-1", false},
		{"trimmed", "  ticket-1  \n", "ticket-1", false},
		{"eof without newline", "ticket-1", "ticket-1", false},
		{"empty line", "\n", "", false},
		{"too long", strings.Repeat("x", stdioTicketMax+1) + "\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			got, err := readStdioTicket(strings.NewReader(tc.in))
			if tc.wantErr {
				c.Require().Error(err, "a line past the cap was accepted")
				return
			}
			c.Require().NoError(err, "readStdioTicket")
			c.Eq(tc.want, got, "ticket")
		})
	}

	conn := &stdioConn{in: strings.NewReader(""), out: io.Discard}
	opts := stdioConnectOptions(conn, "ticket-from-stdin", http.NewServeMux())
	c.Eq("ticket-from-stdin", opts.Ticket, "the ticket must ride the ConnectOptions, not argv")
	c.True(opts.Conn == conn, "the stdio connection must be the injected one")
	c.Eq("", opts.Addr, "nothing is dialled")
	c.Eq("", opts.SocketPath, "nothing is dialled")
	c.Eq("", opts.Credential, "no durable credential")
	c.Eq("", opts.CredentialFile, "nothing is written to disk")
	c.Eq("", opts.EnrollToken, "no enrollment")
	c.Eq(version.String(), opts.SelfReported["version"], "self-reported version")

	// There is no flag that could put a ticket in argv, and the ticket this
	// test used is not in this process's own argv.
	serve := newExecutorServeCmd()
	c.True(serve.Flags().Lookup("ticket") == nil, "no --ticket flag may exist")
	c.NotStrContains(strings.Join(os.Args, " "), "ticket-from-stdin",
		"the ticket must never appear in argv")
}

func TestServeStdioRefusesTransportFlags(t *testing.T) {
	c := assert.NewAborting(t)

	clean := newExecutorServeCmd()
	c.Require().NoError(validateStdioFlags(clean, "install-1"), "a plain --stdio invocation must be valid")

	for _, name := range []string{"connect", "connect-socket", "credential", "credential-file", "enroll-token"} {
		cmd := newExecutorServeCmd()
		c.Require().NoError(cmd.Flags().Set(name, "something"), "set --%s", name)
		err := validateStdioFlags(cmd, "install-1")
		c.Require().Error(err, "--stdio with --%s was accepted", name)
		c.StrContains(err.Error(), "--"+name, "the refusal must name the flag")
	}

	missing := newExecutorServeCmd()
	err := validateStdioFlags(missing, "")
	c.Require().Error(err, "--stdio without --install-id was accepted")
	c.StrContains(err.Error(), "--install-id", "the refusal must name the flag")
}

func TestServeStdioLogsNeverReachStdout(t *testing.T) {
	c := assert.NewAborting(t)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The seam itself: a log line goes to the writer the stdio path pins, and
	// never to the protocol writer.
	var logs bytes.Buffer
	setupStdioLogging(&logs)
	slog.Warn("a diagnostic", "detail", "stderr-only")
	c.StrContains(logs.String(), "a diagnostic", "the log line must reach stderr")

	// And end to end: drive the real path against a daemon that refuses, and
	// assert everything written to stdout is the upgrade request — a log line
	// in front of it would be parsed as a frame.
	client, daemon := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = daemon.Close()
	})

	var stdout bytes.Buffer
	requestSeen := make(chan string, 1)
	go func() {
		_, _ = io.WriteString(daemon, "ticket-1\n")
		br := bufio.NewReader(daemon)
		req, err := http.ReadRequest(br)
		if err != nil {
			requestSeen <- "request parse failed: " + err.Error()
			return
		}
		requestSeen <- req.URL.Path
		_, _ = io.WriteString(daemon, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\n\r\nnope")
	}()

	runErr := make(chan error, 1)
	go func() {
		runErr <- runStdioServe(context.Background(), client, io.MultiWriter(&stdout, client),
			"install-1", http.NewServeMux())
	}()

	select {
	case got := <-requestSeen:
		c.Eq("/executor/connect", got, "the first bytes on stdout must be the upgrade request")
	case <-time.After(5 * time.Second):
		c.Fail("the daemon never received a request")
	}
	select {
	case err := <-runErr:
		c.Require().Error(err, "a refused upgrade must end the stdio serve")
	case <-time.After(5 * time.Second):
		c.Fail("runStdioServe hung after a refusal")
	}

	written := stdout.String()
	c.NotStrContains(written, "level=", "a log line reached stdout")
	c.NotStrContains(written, "msg=", "a log line reached stdout")
}

func TestServeStdioWritesMarkerAfterUpgrade(t *testing.T) {
	c := assert.NewAborting(t)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	paths.ResetInstanceForTest()
	t.Cleanup(paths.ResetInstanceForTest)

	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("HOME", t.TempDir())

	dir := filepath.Join(dataHome, "rafiki")
	c.Require().NoError(os.MkdirAll(dir, 0o700), "mkdir the data dir")
	markerPath := filepath.Join(dir, "installed-executor")
	c.Require().NoError(os.WriteFile(markerPath, []byte("an-older-install\n"), 0o600), "old marker")

	clientConn, daemonConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = daemonConn.Close()
	})

	const ticket = "ticket-from-stdin"
	authSeen := make(chan string, 1)
	daemonErr := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(daemonConn, ticket+"\n"); err != nil {
			daemonErr <- err
			return
		}
		br := bufio.NewReader(daemonConn)
		req, err := http.ReadRequest(br)
		if err != nil {
			daemonErr <- fmt.Errorf("read upgrade request: %w", err)
			return
		}
		authSeen <- req.Header.Get("Authorization")
		// A hand-written 101: this test is about the executor side, and the
		// handshake's own rules are pinned by pkg/upgradeconn.
		if _, err := io.WriteString(daemonConn,
			"HTTP/1.1 101 Switching Protocols\r\n"+
				"Upgrade: executor\r\n"+
				"Connection: Upgrade\r\n"+
				protocol.EpochHeader+": "+strconv.Itoa(protocol.Epoch)+"\r\n\r\n"); err != nil {
			daemonErr <- err
			return
		}
		tr := &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
				return daemonConn, nil
			},
		}
		defer tr.CloseIdleConnections()
		daemon := executorpbconnect.NewExecutorServiceClient(&http.Client{Transport: tr}, "http://executor")
		_, err = daemon.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
		daemonErr <- err
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := executor.NewServer(executor.Options{Root: t.TempDir()})
	defer func() { _ = srv.Close() }()

	runErr := make(chan error, 1)
	go func() {
		runErr <- runStdioServe(ctx, clientConn, clientConn, "install-42", executorHandler(srv, nil))
	}()

	select {
	case err := <-daemonErr:
		c.Require().NoError(err, "the daemon's Describe over the stdio connection")
	case <-time.After(10 * time.Second):
		c.Fail("the daemon never completed the upgrade")
	}
	c.Eq("Ticket "+ticket, <-authSeen, "the ticket must arrive on the upgrade request, read from stdin")

	marker, err := os.ReadFile(markerPath)
	c.Require().NoError(err, "the install marker must be written after the upgrade")
	c.Eq("install-42\n", string(marker), "the marker must hold this install id")
	info, err := os.Stat(markerPath)
	c.Require().NoError(err, "stat the marker")
	c.Eq(os.FileMode(0o600), info.Mode().Perm(), "the marker is 0600")

	cancel()
	select {
	case err := <-runErr:
		c.Require().NoError(err, "a cancelled session ends cleanly")
	case <-time.After(10 * time.Second):
		c.Fail("runStdioServe never returned after cancellation")
	}
}
