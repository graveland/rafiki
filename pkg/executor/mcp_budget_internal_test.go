// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// The concurrency cap is per NAME across every stream on this machine, not per
// stream: a shared laptop can have ten children retrying at once.
func TestMCPBudgetCapsConcurrentStreams(t *testing.T) {
	c := assert.NewCollecting(t)
	b := &mcpBudget{}
	for i := 0; i < mcpMaxStreamsPerName; i++ {
		release, err := b.acquire("hesabu")
		c.Require().NoError(err, "acquire %d", i+1)
		c.Require().True(release != nil, "acquire %d returned no release", i+1)
	}
	_, err := b.acquire("hesabu")
	c.Require().Error(err, "the ninth concurrent stream was admitted")
	c.StrContains(err.Error(), "already has 8 live streams", "error")
}

// A crash loop is tripped by quick failures inside the window and lifts when
// the cool-off elapses — an hourly crash is not a crash loop, so a process
// that ran past mcpQuickFailureWindow clears the record instead.
func TestMCPBudgetTripsAndRecovers(t *testing.T) {
	c := assert.NewCollecting(t)
	b := &mcpBudget{}
	release, err := b.acquire("hesabu")
	c.Require().NoError(err, "acquire")
	release()

	for i := 0; i < mcpCrashLoopThreshold; i++ {
		b.noteFailure("exited after 3ms: boom")
	}
	_, err = b.acquire("hesabu")
	c.Require().Error(err, "a crash-looping server was admitted")
	c.StrContains(err.Error(), "boom", "error carries the last failure")

	// Recovery: past the cool-off, a spawn is admitted again.
	b.mu.Lock()
	b.refuse = time.Now().Add(-time.Second)
	b.mu.Unlock()
	release, err = b.acquire("hesabu")
	c.Require().NoError(err, "admitted after the cool-off")
	release()

	// A long-lived process resets the record: two more quick failures then a
	// success must not trip the cool-off.
	for i := 0; i < mcpCrashLoopThreshold-1; i++ {
		b.noteFailure("exited after 3ms: boom")
	}
	b.noteSuccess()
	for i := 0; i < mcpCrashLoopThreshold-1; i++ {
		b.noteFailure("exited after 3ms: boom")
	}
	release, err = b.acquire("hesabu")
	c.Require().NoError(err, "a run long enough to outlive the window did not reset the record")
	release()
}

func TestMCPBudgetForgetsFailuresOutsideTheWindow(t *testing.T) {
	c := assert.NewCollecting(t)
	b := &mcpBudget{}
	b.mu.Lock()
	b.failAt = []time.Time{time.Now().Add(-2 * mcpCrashLoopWindow), time.Now().Add(-2 * mcpCrashLoopWindow)}
	b.mu.Unlock()
	for i := 0; i < mcpCrashLoopThreshold-1; i++ {
		b.noteFailure("boom")
	}
	release, err := b.acquire("hesabu")
	c.Require().NoError(err, "stale failures counted against us")
	release()
}

func TestSplitMCPCommandArgv(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"hesabu mcp --stdio", []string{"hesabu", "mcp", "--stdio"}},
		{"  a   b  ", []string{"a", "b"}},
		{`a\ b c`, []string{"a b", "c"}},
		{`a\\b`, []string{`a\b`}},
		// Quotes are not separators — only an escaped space joins words, and
		// an escaped quote is just that character.
		{`say \"hi\"`, []string{"say", `"hi"`}},
		{"a\tb\nc", []string{"a", "b", "c"}},
		// No shell: these are literal text, which is the point of not
		// involving one.
		{"echo $HOME *.go", []string{"echo", "$HOME", "*.go"}},
	}
	for _, tc := range cases {
		got, err := splitMCPCommandArgv(tc.in)
		assert.NewAborting(t).EqDeep(got, tc.want, "split(%q) (err %v)", tc.in, err)
	}
}

func TestMCPStderrRingKeepsTheLastBytes(t *testing.T) {
	c := assert.NewCollecting(t)
	var r mcpStderrRing
	r.append([]byte("first line\n"))
	r.append([]byte("last line\n"))
	c.Eq(r.tail(), "first line\nlast line\n", "tail")

	r.append([]byte(make([]byte, mcpStderrRingMax)))
	c.Eq(len(r.tail()), mcpStderrRingMax, "tail is capped")
	r.append([]byte("\nfinal: disk full\n"))
	c.StrContains(r.tail(), "final: disk full", "tail carries the last thing the server said")
}

// A kill this executor performed is NEITHER a failure nor a success: three
// ordinary stream ends (a daemon disconnect, a killed child, a rolling pod)
// must not quarantine a healthy server for the whole machine, and a kill we
// performed must not clear a real crash-loop record either.
func TestMCPBudgetIgnoresOurOwnKills(t *testing.T) {
	c := assert.NewCollecting(t)
	b := &mcpBudget{}

	// Three of our own kills, each far inside the quick-failure window.
	for i := 0; i < mcpCrashLoopThreshold; i++ {
		b.noteExit(true, time.Millisecond, "(signal terminated): ")
	}
	release, err := b.acquire("hesabu")
	c.Require().NoError(err, "an ordinary stream end was counted as a crash")
	release()

	// A kill does not RESET the record either: two quick failures on either
	// side of one are still three quick failures.
	b.noteExit(false, time.Millisecond, "(exit code 1): boom")
	b.noteExit(true, time.Millisecond, "(signal terminated): ")
	b.noteExit(false, time.Millisecond, "(exit code 1): boom")
	release, err = b.acquire("hesabu")
	c.Require().NoError(err, "two failures are not a crash loop")
	release()
	b.noteExit(false, time.Millisecond, "(exit code 1): boom")
	_, err = b.acquire("hesabu")
	c.Require().Error(err, "a third quick failure was not counted")
	c.StrContains(err.Error(), "boom", "error carries the last stderr tail")

	// A process that outlived the window resets the record.
	b2 := &mcpBudget{}
	b2.noteExit(false, time.Millisecond, "(exit code 1): boom")
	b2.noteExit(false, time.Millisecond, "(exit code 1): boom")
	b2.noteExit(false, mcpQuickFailureWindow+time.Second, "(exit code 1): boom")
	for i := 0; i < mcpCrashLoopThreshold-1; i++ {
		b2.noteExit(false, time.Millisecond, "(exit code 1): boom")
	}
	release, err = b2.acquire("hesabu")
	c.Require().NoError(err, "a run long enough to outlive the window did not reset the record")
	release()
}
