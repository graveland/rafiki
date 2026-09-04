package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/daraja"
)

// daraja is a SUBCOMMAND of rafiki, not a third binary: this repo ships exactly
// two artifacts and cmd/rafiki-executor was deleted to keep it that way.
func TestDarajaIsRegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "daraja") {
			return
		}
	}
	t.Fatal("daraja is not registered on the root command")
}

func TestDarajaServeRequiresSocketAndBinary(t *testing.T) {
	cmd := newDarajaCmd()
	serve := cmd.Commands()
	if len(serve) == 0 {
		t.Fatal("daraja has no subcommands; expected `serve`")
	}
	for _, flag := range []string{"socket", "binary", "cwd", "kind", "model", "resume", "permission-mode"} {
		if serve[0].Flags().Lookup(flag) == nil {
			t.Errorf("daraja serve is missing the --%s flag", flag)
		}
	}
}

// The serve loop must observe host.Done(): the respawn cap closing done is the
// host giving up, and a serve that keeps running then hosts nothing while its
// socket stays bound. Driven against the real serveLoop rather than a rebuilt
// select because the fix is one arm of that select.
func TestServeLoopExitsWhenHostGivesUp(t *testing.T) {
	// A child that ignores claude's flags and exits instantly: two instant
	// exits plus the 10ms backoff trip RespawnLimit=1 in milliseconds, where
	// the defaults would take ~10s.
	bin := filepath.Join(t.TempDir(), "dying-child")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := daraja.NewHost(daraja.HostOptions{
		Binary:         bin,
		RespawnLimit:   1,
		RespawnBackoff: 10 * time.Millisecond,
		Spec:           daraja.ChildSpec{Kind: daraja.KindClaude},
	})
	if err := host.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	srv := daraja.NewServer(host)
	path, handler := srv.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: mux, Protocols: protos}

	// macOS caps a unix socket path at ~104 chars, and t.TempDir() nests the
	// test name under /var/folders — so the listener gets a short dir of its
	// own.
	dir, err := os.MkdirTemp("", "dserve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}

	exited := make(chan error, 1)
	go func() { exited <- serveLoop(host, srv, httpSrv, ln) }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serveLoop returned an error on the give-up path: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serveLoop is still running after the host gave up; the Done arm is not in the select")
	}
}
