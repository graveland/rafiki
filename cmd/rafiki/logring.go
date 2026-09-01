// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// logRing is a bounded, in-memory slog.Handler.
//
// cmd/rafiki never called slog.SetDefault, so everything the session executor
// logs -- pkg/execpool alone has around ten join, park, health and reconnect
// messages -- went to the default handler, which is stderr, which is the alt
// screen the cockpit is drawing on.
//
// Capture rather than throttle. Raising the level to warn leaves the park and
// reconnect chatter on the screen, and those are exactly the messages worth
// keeping. Nothing reaches the terminal from here, so the level stays at info.
//
// This is also what a log pane will read: it is in-process and already
// bounded, so the pane is a rendering decision later rather than new plumbing.
type logRing struct {
	mu      sync.Mutex
	records []string
	cap     int
	// mirror receives every record as well, when RAFIKI_TUI_LOG named a file.
	// A file only earns its place after the process is gone, so it is opt-in.
	mirror *os.File
}

func newLogRing(capacity int) *logRing {
	if capacity < 1 {
		capacity = 1
	}
	return &logRing{cap: capacity}
}

func (r *logRing) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRing) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *logRing) WithGroup(string) slog.Handler { return r }

func (r *logRing) Handle(_ context.Context, rec slog.Record) error {
	var sb strings.Builder
	sb.WriteString(rec.Time.Format("15:04:05.000"))
	sb.WriteString(" ")
	sb.WriteString(rec.Level.String())
	sb.WriteString(" ")
	sb.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	line := sb.String()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, line)
	if len(r.records) > r.cap {
		r.records = r.records[len(r.records)-r.cap:]
	}
	if r.mirror != nil {
		fmt.Fprintln(r.mirror, line)
	}
	return nil
}

// Records returns a copy of the ring, oldest first.
func (r *logRing) Records() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.records))
	copy(out, r.records)
	return out
}

// Dump writes the ring to stderr. Call it only AFTER the alt screen is torn
// down: a dying executor that logged nothing anywhere is worse than a
// corrupted screen, which is the failure this whole mechanism replaced.
func (r *logRing) Dump() {
	for _, line := range r.Records() {
		fmt.Fprintln(os.Stderr, line)
	}
}

// tuiLogEnv names the opt-in file mirror.
const tuiLogEnv = "RAFIKI_TUI_LOG"

// installTUILogging makes the ring the process default logger and returns it
// with a cleanup that restores the previous default.
func installTUILogging(capacity int) (*logRing, func(), error) {
	r := newLogRing(capacity)
	if path := os.Getenv(tuiLogEnv); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("%s=%q: %w", tuiLogEnv, path, err)
		}
		r.mirror = f
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(r))
	return r, func() {
		slog.SetDefault(prev)
		if r.mirror != nil {
			_ = r.mirror.Close()
		}
	}, nil
}
