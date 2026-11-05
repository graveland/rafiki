// SPDX-License-Identifier: Apache-2.0

// Command claudedriver drives ONE real Claude Code session for the delivery
// capture (testdata/claude-capture-delivery), because `rafiki claude
// --turn-file` sends its turns strictly one after another and the case this
// records needs a message that arrives WHILE a turn is running.
//
// It is a recording driver, not production code and not a test: it starts the
// real binary with the headless stream-json flags, writes frames on a schedule
// derived from what the binary prints, and echoes every line it reads so the
// session is visible in the terminal while it runs.
//
// The session it produces, in order:
//
//  1. an ordinary text turn;
//  2. a message sent MID-TURN (while turn 1 is waiting on a tool result), which
//     Claude Code folds into the running turn wrapped in its own prose — the
//     shape the delivery acknowledgement exists to replace;
//  3. two DELIBERATE identical sends, each with its own uuid. They must be two
//     messages, not one deduplicated repeat: the uuid, not the text, is the
//     identity.
//
// Every frame carries a uuid the daemon would mint, so the recording shows
// what --replay-user-messages actually echoes and what the captured request
// bodies look like.
//
//	go run ./test/integration/fixtures/claudedriver --claude /path/to/claude --model haiku
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func main() {
	claude := flag.String("claude", "claude", "path to the claude binary")
	model := flag.String("model", "haiku", "model id for the session")
	timeout := flag.Duration("timeout", 5*time.Minute, "hard cap on the whole session")
	extra := flag.String("extra", "", "extra claude arguments, space separated (e.g. "+
		"\"--strict-mcp-config --mcp-config {\\\"mcpServers\\\":{}}\"); they are appended verbatim")
	flag.Parse()

	if err := run(*claude, *model, *timeout, strings.Fields(*extra)); err != nil {
		fmt.Fprintln(os.Stderr, "claudedriver:", err)
		os.Exit(1)
	}
}

// step is what the driver does next, keyed on the binary's own output.
type step int

const (
	stepWaitToolUse step = iota
	stepWaitResults
	stepDone
)

func run(claude, model string, timeout time.Duration, extra []string) error {
	argv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--replay-user-messages",
		"--dangerously-skip-permissions",
		"--model", model,
	}
	argv = append(argv, extra...)
	cmd := exec.Command(claude, argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	// The binary prints NOTHING until its first message arrives (verified:
	// zero bytes with stdin open and no input), so the first turn goes out
	// immediately rather than waiting for a startup frame that never comes.
	d := &driver{stdin: stdin, out: os.Stdout, phase: stepWaitToolUse}
	if err := d.write("Use the Bash tool to run `sleep 3`. Then reply with exactly one word: ready"); err != nil {
		return err
	}
	deadline := time.AfterFunc(timeout, func() { d.fail("session timed out") })
	defer deadline.Stop()
	scanErr := d.scan(stdout)
	if err := cmd.Wait(); err != nil && scanErr == nil {
		scanErr = fmt.Errorf("claude exited: %w", err)
	}
	return scanErr
}

// driver holds the session state. The scan goroutine is the only writer of
// these fields; the timer above only reports.
type driver struct {
	stdin io.WriteCloser
	out   io.Writer

	mu      sync.Mutex
	phase   step
	results int
	writes  int
	err     error
}

// fail records a fatal condition; the scan loop returns it.
func (d *driver) fail(msg string) {
	d.mu.Lock()
	if d.err == nil {
		d.err = fmt.Errorf("%s", msg)
	}
	d.mu.Unlock()
	_ = d.stdin.Close()
}

// write sends one user frame and prints what it sent, so the recording's
// reader can pair a frame with the echo it produced.
func (d *driver) write(text string) error {
	uuid := newUUID()
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"uuid":    uuid,
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	d.writes++
	fmt.Fprintf(os.Stderr, "claudedriver: → %s  uuid=%s\n", text, uuid)
	if _, err := d.stdin.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// scan reads the binary's stdout and drives the schedule. Every line is echoed
// verbatim so the session can be watched, and a result line advances the
// session one message.
func (d *driver) scan(stdout io.Reader) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		_, _ = d.out.Write(append(line, '\n'))

		var f struct {
			Type     string `json:"type"`
			IsReplay bool   `json:"isReplay"`
			Message  *struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}

		switch {
		case f.Type == "assistant" && d.phase == stepWaitToolUse:
			// The turn is genuinely running now (a tool call is in flight), so a
			// message sent here is a MID-TURN message.
			for _, b := range f.Message.Content {
				if b.Type == "tool_use" {
					d.phase = stepWaitResults
					if err := d.write("Reply with exactly one word: also"); err != nil {
						return err
					}
					break
				}
			}
		case f.Type == "result":
			d.results++
			switch d.results {
			case 1:
				// Turn one (and the mid-turn message folded into it) is done.
				if err := d.write("Reply with exactly one word: twice"); err != nil {
					return err
				}
			case 2:
				// The first of the two identical sends has landed; repeat it
				// verbatim with a fresh uuid.
				if err := d.write("Reply with exactly one word: twice"); err != nil {
					return err
				}
			default:
				d.phase = stepDone
				_ = d.stdin.Close()
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	if d.results < 3 {
		return fmt.Errorf("session ended after %d result frame(s); want 3", d.results)
	}
	return nil
}

// newUUID mints a v4 uuid — the shape the daemon's uuidv7 has on the wire, and
// all that matters for the recording is that each frame carries a distinct one.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
