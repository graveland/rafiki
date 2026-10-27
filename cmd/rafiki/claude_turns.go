// SPDX-License-Identifier: Apache-2.0

// Scripted multi-turn Claude Code sessions.
//
// `rafiki claude` normally hands the TTY to an interactive session. With turns
// supplied it instead runs the SAME binary in the headless stream-json mode the
// daemon uses for a claude child (pkg/claudeargv's ModeHeadless), writes each
// turn to its stdin and waits for that turn's result frame. That is what makes
// a multi-turn capture possible: a tool-call turn and the follow-up that reads
// its output are two prompts in ONE session, with one conversation and one
// session id, instead of three independent `-p` runs.
//
// The wire format is not reimplemented here: pkg/child's ClaudeProvider encodes
// the outbound user message, so the frames this writes are byte-identical to
// the ones a daemon-managed claude child receives.

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/child"
)

// claudeTurnTimeout is the default ceiling on ONE turn. A tool-call turn on a
// slow model can legitimately take minutes, so this is generous; it exists to
// turn a wedged session into an error rather than a hung capture.
const claudeTurnTimeout = 10 * time.Minute

// claudeTurnsOptions is one scripted session: the invocation to run and the
// prompts to send it, in order.
type claudeTurnsOptions struct {
	Env     []string
	Args    []string
	Turns   []string
	Out     io.Writer
	Err     io.Writer
	Timeout time.Duration
	// Interrupt, when set to claudeInterruptToolUse, ends the run by killing
	// claude once the last turn's response carries a tool_use block. The kill
	// is the run's successful end; a last turn that finishes without a tool
	// call is an error.
	Interrupt string
	// Binary is the claude executable; a variable so tests drive a fixture
	// instead of a real session.
	Binary string
}

// claudeTurnFrame is the part of claude's stream-json output this driver reads:
// assistant text to echo, and the result frame that ends a turn.
type claudeTurnFrame struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Result string `json:"result"`
}

// runClaudeTurns runs the session and returns when the last turn has finished
// and the process has exited.
func runClaudeTurns(o claudeTurnsOptions) error {
	if len(o.Turns) == 0 {
		return errors.New("no turns: pass --turn or --turn-file")
	}
	if o.Timeout <= 0 {
		o.Timeout = claudeTurnTimeout
	}
	if o.Binary == "" {
		o.Binary = "claude"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}

	cmd := exec.Command(o.Binary, o.Args...) //nolint:gosec // the operator's own claude
	cmd.Env = o.Env
	// Its own process group, so a kill takes the tool subprocesses with it: a
	// killed claude otherwise leaves its Bash command running, reparented to
	// init, for as long as the command takes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = o.Err
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claude stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", o.Binary, err)
	}

	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		// A single stream-json line can carry a whole tool result, so the
		// default 64KiB token ceiling is not enough.
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		readErr <- scanner.Err()
		close(lines)
	}()

	provider := child.ClaudeProvider{}
	for i, turn := range o.Turns {
		if err := writeClaudeTurn(stdin, provider, turn); err != nil {
			killAndReap(cmd)
			return fmt.Errorf("turn %d: %w", i+1, err)
		}
		interrupt := o.Interrupt != "" && i == len(o.Turns)-1
		interrupted, err := awaitClaudeTurnResult(lines, o.Out, o.Timeout, interrupt)
		if err != nil {
			killAndReap(cmd)
			return fmt.Errorf("turn %d: %w", i+1, err)
		}
		if interrupt {
			if !interrupted {
				killAndReap(cmd)
				return fmt.Errorf("turn %d: interrupt %q requested but the turn finished without a tool call", i+1, o.Interrupt)
			}
			killAndReap(cmd)
			return nil
		}
	}
	// Closing stdin is how claude learns the session is over; waiting then
	// bounds the whole run on the process rather than on a frame.
	if err := stdin.Close(); err != nil {
		killAndReap(cmd)
		return fmt.Errorf("close claude stdin: %w", err)
	}
	select {
	case err := <-readErr:
		if err != nil && !errors.Is(err, io.EOF) {
			killAndReap(cmd)
			return fmt.Errorf("read claude output: %w", err)
		}
	case <-time.After(o.Timeout):
		// The last turn answered, but the process is not going away: a session
		// that never exits is a failed capture, not a reason to hang the caller.
		killAndReap(cmd)
		return fmt.Errorf("claude did not exit within %s after the last turn", o.Timeout)
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("claude exited %d", exitErr.ExitCode())
		}
		return fmt.Errorf("wait for claude: %w", err)
	}
	return nil
}

// killAndReap stops a session that cannot finish on its own and collects it, so
// a failed turn does not leave a zombie behind.
//
// The reap is bounded: a session wedged in a way that outlives SIGKILL (a shell
// waiting on a child of its own) must not turn a failed turn into a hung one.
func killAndReap(cmd *exec.Cmd) {
	// The negative pid signals the whole group; Setpgid made claude its leader.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// writeClaudeTurn sends one prompt as a stream-json user message, encoded by the
// same provider a daemon-managed claude child uses.
func writeClaudeTurn(w io.Writer, provider child.ClaudeProvider, turn string) error {
	frame, err := json.Marshal(struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{Type: "prompt", Message: turn})
	if err != nil {
		return err
	}
	out := provider.EncodeOutbound(frame)
	if len(out) == 0 {
		return errors.New("claude provider refused the prompt frame")
	}
	if _, err := w.Write(append(out, '\n')); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	return nil
}

// awaitClaudeTurnResult reads frames until this turn's result arrives, echoing
// assistant text as it goes. A turn that produces an error result is an error,
// not a silent success. With stopOnToolUse it returns (true, nil) as soon as an
// assistant frame carries a tool_use block, before any result.
func awaitClaudeTurnResult(lines <-chan string, out io.Writer, timeout time.Duration, stopOnToolUse bool) (bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return false, errors.New("claude closed its output before the turn finished")
			}
			var frame claudeTurnFrame
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				// Not every line is JSON worth parsing; a stream that carries a
				// stray line is not a reason to fail the session.
				continue
			}
			switch frame.Type {
			case "assistant":
				echoClaudeText(out, frame)
				if stopOnToolUse && hasToolUse(frame) {
					return true, nil
				}
			case "result":
				if frame.IsError {
					msg := strings.TrimSpace(frame.Result)
					if msg == "" {
						msg = frame.Subtype
					}
					return false, fmt.Errorf("claude reported an error: %s", msg)
				}
				return false, nil
			}
		case <-deadline.C:
			return false, fmt.Errorf("no result frame within %s", timeout)
		}
	}
}

func hasToolUse(frame claudeTurnFrame) bool {
	for _, block := range frame.Message.Content {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// echoClaudeText prints the assistant's text blocks, which is what makes a
// scripted run readable while it happens rather than only afterwards.
func echoClaudeText(out io.Writer, frame claudeTurnFrame) {
	for _, block := range frame.Message.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		_, _ = fmt.Fprintln(out, block.Text)
	}
}

// claudeTurnsFromFlags collects the scripted prompts: --turn in order, then
// --turn-file's lines. A file is the practical way to carry a tool-call turn and
// the follow-up that depends on it, and it can be checked in as a fixture.
func claudeTurnsFromFlags(cmd *cobra.Command) ([]string, error) {
	turns, _ := cmd.Flags().GetStringArray("turn")
	file, _ := cmd.Flags().GetString("turn-file")
	explicit, _ := cmd.Flags().GetBool("turns")

	if file != "" {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, fmt.Errorf("read --turn-file %s: %w", file, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			turns = append(turns, line)
		}
	}
	if explicit && len(turns) == 0 {
		return nil, errors.New("--turns needs at least one --turn or a --turn-file")
	}
	return turns, nil
}

// envFirst returns the first non-empty value among keys, so a scripted run can
// take the shell's Anthropic credential without naming which variable holds it.
func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
