// SPDX-License-Identifier: Apache-2.0

// Multi-run Claude Code scripts.
//
// A scripted session (claude_turns.go) is ONE claude process. Some behaviour
// only exists across processes: a session interrupted mid-tool-call and then
// resumed by a second process is the case that motivated this. A script is a
// TOML file of runs, each a process with its own turns; a run can resume an
// earlier run's session and can be killed on purpose.
//
// --turn and --turn-file stay a one-run script with no name, so a single-session
// capture is unchanged.

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// claudeInterruptToolUse is the only interrupt a run can ask for: kill the
// process the moment the last turn's response carries a tool_use block, before
// the tool result goes back. The kill is immediate on purpose. Given even a
// quarter second Claude Code persists that assistant message and, on resume,
// repairs it with a synthetic tool_result; killed at the frame, the unanswered
// tool_use is dropped from the resumed history and the next prompt gets a
// synthetic "No response requested." in its place, which is the rewrite of a
// message the proxy already stored.
const claudeInterruptToolUse = "tool_use"

// claudeScript is a TOML script. Top-level model, system_prompt, tools,
// turn_timeout and args are defaults for every run; a run overrides the
// first four and appends to args.
type claudeScript struct {
	Model        string         `toml:"model"`
	SystemPrompt *string        `toml:"system_prompt"`
	Tools        *string        `toml:"tools"`
	TurnTimeout  scriptDuration `toml:"turn_timeout"`
	Args         []string       `toml:"args"`
	Runs         []claudeRun    `toml:"run"`
}

// claudeRun is one claude process.
type claudeRun struct {
	Name         string         `toml:"name"`
	Resume       string         `toml:"resume"`
	Turns        []string       `toml:"turns"`
	Interrupt    string         `toml:"interrupt"`
	Model        string         `toml:"model"`
	SystemPrompt *string        `toml:"system_prompt"`
	Tools        *string        `toml:"tools"`
	TurnTimeout  scriptDuration `toml:"turn_timeout"`
	Args         []string       `toml:"args"`
}

// scriptDuration is a Go duration string ("20s") in TOML.
type scriptDuration time.Duration

func (d *scriptDuration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	if v <= 0 {
		return fmt.Errorf("duration %q must be positive", b)
	}
	*d = scriptDuration(v)
	return nil
}

// claudeDriverFlags are the flags the driver sets itself, from the run's name,
// resume and persistence, or from the structured fields. Letting a script pass
// one in args would fight the driver's own value.
var claudeDriverFlags = []string{
	"-p", "--print", "--input-format", "--output-format", "--verbose",
	"--resume", "-r", "--continue", "-c", "--session-id", "--fork-session",
	"--no-session-persistence",
	"--model", "--system-prompt", "--tools",
}

// parseClaudeScript decodes and validates a script. Unknown keys are an error:
// a misspelt `interupt` would otherwise record a session that was never
// interrupted.
func parseClaudeScript(data []byte) (*claudeScript, error) {
	var s claudeScript
	md, err := toml.Decode(string(data), &s)
	if err != nil {
		return nil, fmt.Errorf("parse script: %w", err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("parse script: unknown key(s): %s", strings.Join(keys, ", "))
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *claudeScript) validate() error {
	if len(s.Runs) == 0 {
		return errors.New("script has no [[run]]")
	}
	if err := checkDriverFlags("args", s.Args); err != nil {
		return err
	}
	earlier := map[string]bool{}
	resumed := map[string]bool{}
	for i, r := range s.Runs {
		label := fmt.Sprintf("run %d", i+1)
		if r.Name != "" {
			label = fmt.Sprintf("run %q", r.Name)
		}
		if len(r.Turns) == 0 {
			return fmt.Errorf("%s: no turns", label)
		}
		if r.Name != "" && earlier[r.Name] {
			return fmt.Errorf("%s: name used twice", label)
		}
		if r.Resume != "" {
			if !earlier[r.Resume] {
				return fmt.Errorf("%s: resume %q names no earlier run", label, r.Resume)
			}
			if resumed[r.Resume] {
				return fmt.Errorf("%s: run %q is already resumed by another run", label, r.Resume)
			}
			resumed[r.Resume] = true
		}
		switch r.Interrupt {
		case "", claudeInterruptToolUse:
		default:
			return fmt.Errorf("%s: interrupt %q is not supported (want %q)", label, r.Interrupt, claudeInterruptToolUse)
		}
		if err := checkDriverFlags(label+" args", r.Args); err != nil {
			return err
		}
		if r.Name != "" {
			earlier[r.Name] = true
		}
	}
	return nil
}

func checkDriverFlags(where string, args []string) error {
	for _, a := range args {
		flag, _, _ := strings.Cut(a, "=")
		for _, banned := range claudeDriverFlags {
			if flag == banned {
				return fmt.Errorf("%s: %s is set by the driver; use the script's own fields", where, banned)
			}
		}
	}
	return nil
}

// claudeRunPlan is one run with every default resolved and its session decided.
type claudeRunPlan struct {
	Name         string
	SessionID    string
	Resume       bool
	Persist      bool
	Model        string
	SystemPrompt string
	Tools        string
	Timeout      time.Duration
	Args         []string
	Turns        []string
	Interrupt    string
}

// claudeScriptDefaults are the flag values a script's own fields override.
type claudeScriptDefaults struct {
	Model        string
	SystemPrompt string
	Tools        string
	Timeout      time.Duration
	// SessionID names the first run's session when the operator passed
	// --session; every other session id is minted.
	SessionID string
}

// plan resolves the runs in order. A run that resumes another keeps that
// run's session id, so both land on one conversation at the proxy
// (X-Rafiki-Session). A run is persisted only when it takes part in a resume;
// every other run keeps the capture default of leaving no session file behind.
func (s *claudeScript) plan(d claudeScriptDefaults) []claudeRunPlan {
	resumedBy := map[string]bool{}
	for _, r := range s.Runs {
		if r.Resume != "" {
			resumedBy[r.Resume] = true
		}
	}
	sessions := map[string]string{}
	plans := make([]claudeRunPlan, 0, len(s.Runs))
	for i, r := range s.Runs {
		p := claudeRunPlan{
			Name:         r.Name,
			Resume:       r.Resume != "",
			Persist:      r.Resume != "" || resumedBy[r.Name],
			Model:        firstNonEmpty(r.Model, s.Model, d.Model),
			SystemPrompt: firstSet(r.SystemPrompt, s.SystemPrompt, d.SystemPrompt),
			Tools:        firstSet(r.Tools, s.Tools, d.Tools),
			Timeout:      firstDuration(time.Duration(r.TurnTimeout), time.Duration(s.TurnTimeout), d.Timeout),
			Args:         append(append([]string{}, s.Args...), r.Args...),
			Turns:        r.Turns,
			Interrupt:    r.Interrupt,
		}
		switch {
		case r.Resume != "":
			p.SessionID = sessions[r.Resume]
		case i == 0 && d.SessionID != "":
			p.SessionID = d.SessionID
		default:
			p.SessionID = uuid.NewString()
		}
		if r.Name != "" {
			sessions[r.Name] = p.SessionID
		}
		plans = append(plans, p)
	}
	return plans
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstSet returns the first set pointer's value, else the fallback: an empty
// string that was written down (tools = "" means no tools) is a value.
func firstSet(a, b *string, fallback string) string {
	switch {
	case a != nil:
		return *a
	case b != nil:
		return *b
	}
	return fallback
}

func firstDuration(vals ...time.Duration) time.Duration {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// claudeScriptFromFlags returns the script to run, or nil for an interactive
// session. --script is exclusive with --turn/--turn-file, which stay a one-run
// script.
func claudeScriptFromFlags(cmd *cobra.Command) (*claudeScript, error) {
	path, _ := cmd.Flags().GetString("script")
	if path == "" {
		turns, err := claudeTurnsFromFlags(cmd)
		if err != nil || len(turns) == 0 {
			return nil, err
		}
		return &claudeScript{Runs: []claudeRun{{Turns: turns}}}, nil
	}
	inline, _ := cmd.Flags().GetStringArray("turn")
	file, _ := cmd.Flags().GetString("turn-file")
	if len(inline) > 0 || file != "" {
		return nil, errors.New("--script cannot be combined with --turn or --turn-file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --script %s: %w", path, err)
	}
	s, err := parseClaudeScript(data)
	if err != nil {
		return nil, fmt.Errorf("--script %s: %w", path, err)
	}
	return s, nil
}
