package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume [id|name]",
		Short: "Resume an exited child, or spawn a new child from any pi session file",
		Long: `Resume a fundi-managed child that has exited, or spawn a fresh child continuing
any pi session — including sessions that were never managed by fundi — by pointing
directly at the session's .jsonl file.

Standard resume (existing behaviour):
  fundi resume [id|name]

Resume from a pi session file (new):
  fundi resume --pi-session <uuid-or-path>

With --pi-session, fundi reads the session's .jsonl to extract cwd, model, and
thinking level, then spawns a new child via ctrl_spawn (NOT ctrl_resume).

The --pi-session argument may be:
  - An absolute or relative path to a .jsonl file
  - A path starting with ~ (tilde is expanded to $HOME)
  - A bare UUID, resolved by globbing ~/.pi/agent/sessions/*/

After spawning, fundi attaches the TUI to the new child, identical to 'fundi create'.
Use --detached to skip attaching.`,
		Args: cobra.MaximumNArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Cobra's mutual-exclusion enforcement fires before RunE, so this
			// check is also testable without a live daemon.
			piSess, _ := cmd.Flags().GetString("pi-session")
			if piSess != "" && len(args) > 0 {
				return errors.New("specify either [id|name] or --pi-session, not both")
			}
			return nil
		},
		RunE: runResume,
	}
	cmd.Flags().String("api-key", "", "Optional API key override for this resume")
	_ = cmd.RegisterFlagCompletionFunc("api-key", cobra.NoFileCompletions)

	// --pi-session path: spawn from a session.jsonl.
	cmd.Flags().String("pi-session", "", "Resume any pi session by path or UUID (spawns a new child via ctrl_spawn)")
	_ = cmd.RegisterFlagCompletionFunc("pi-session", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"jsonl"}, cobra.ShellCompDirectiveFilterFileExt
	})

	// Spawn-time overrides for the --pi-session path.  Reuse addSpawnFlags so
	// buildSpawnRequest handles env-var defaults, labels, and forward-env.
	addSpawnFlags(cmd)

	// Attach-mode flags (same semantics as fundi create).
	cmd.Flags().Bool("detached", false, "Spawn without attaching (--pi-session path only)")
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")

	// Preset support (same semantics as fundi create).
	cmd.Flags().String("preset", "", "Apply a named preset from <config dir>/presets.json")
	_ = cmd.RegisterFlagCompletionFunc("preset", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		pf, err := loadPresets()
		if err != nil || pf == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := make([]string, 0, len(pf.Presets))
		for name := range pf.Presets {
			names = append(names, name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	})

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildrenByState(cmd, toComplete, func(ch protocol.ChildSummary) bool {
			return ch.Status == string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runResume(cmd *cobra.Command, args []string) error {
	piSessionInput, _ := cmd.Flags().GetString("pi-session")

	if piSessionInput != "" {
		// Mutual exclusion with positional args is enforced in PreRunE;
		// we only need to branch here.
		return runResumeFromPiSession(cmd, piSessionInput)
	}

	// Existing path: daemon-side ctrl_resume by child ID.
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	var input string
	if len(args) > 0 {
		input = args[0]
	}
	childID, err := resolveTarget(ctx, c, input)
	if err != nil {
		return err
	}

	apiKey, _ := cmd.Flags().GetString("api-key")

	resp, err := c.Request(ctx, protocol.ResumeRequest{
		Type:    protocol.TypeCtrlResume,
		ChildID: childID,
		APIKey:  apiKey,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_resume: %s", client.FormatError(resp))
	}

	_ = setActive(childID)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}

// buildResumeFromPiSessionRequest builds the SpawnRequest for the --pi-session
// resume path: it resolves the session file, merges it with buildSpawnRequest's
// flag/env-derived request, and applies the preset and resumed-from-session
// label. Pulled out of runResumeFromPiSession as a pure function (no daemon
// dial) so the request construction is testable without a live daemon —
// runResumeFromPiSession already overwrites several request fields after
// buildSpawnRequest returns (Cwd, Model, Provider, Thinking, Type,
// ResumeSession, ResumedFromSession), and every one of those overwrite sites
// is a place a future field could be silently clobbered the same way
// SkillsDirs/MCPConfig were.
func buildResumeFromPiSessionRequest(cmd *cobra.Command, input string) (protocol.SpawnRequest, error) {
	info, err := resolvePiSession(input)
	if err != nil {
		return protocol.SpawnRequest{}, err
	}

	// buildSpawnRequest merges --model > RAFIKI_DEFAULT_MODEL > "" and handles
	// --label, RAFIKI_DEFAULT_LABELS, --forward-env, and all other spawn flags.
	// We pass no positional args — there's no name argument in this path.
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		return protocol.SpawnRequest{}, err
	}

	// cwd: jsonl value is the default; an explicit --cwd flag overrides it.
	if !cmd.Flags().Changed("cwd") {
		req.Cwd = info.Cwd
	}

	// model/provider: jsonl is the fallback when neither --model nor RAFIKI_DEFAULT_MODEL
	// set a value.  Only set Provider when we're also taking Model from the jsonl,
	// since the --model flag embeds the provider as a prefix ("anthropic/model").
	if req.Model == "" {
		if info.Model == "" {
			return protocol.SpawnRequest{}, fmt.Errorf(
				"model required: session %s has no model_change events; set --model or RAFIKI_DEFAULT_MODEL",
				input,
			)
		}
		req.Model = info.Model
		req.Provider = info.Provider
	}

	// thinking: jsonl is the fallback when --thinking is not explicitly set.
	if !cmd.Flags().Changed("thinking") {
		req.Thinking = info.Thinking
	}

	// This is a spawn (not a ctrl_resume); always point ResumeSession at the
	// jsonl path so pi continues from where the session left off.
	req.Type = protocol.TypeCtrlSpawn
	req.ResumeSession = info.Path

	// Merge preset at the lowest priority: preset < env-var defaults < explicit flags.
	// buildSpawnRequest has already merged env-var defaults and --label flags;
	// preset fills in any keys/model that weren't set by higher-priority sources.
	presetName, _ := cmd.Flags().GetString("preset")
	if presetName != "" {
		pf, pErr := loadPresets()
		if pErr != nil {
			return protocol.SpawnRequest{}, fmt.Errorf("--preset: %w", pErr)
		}
		preset, ok := pf.Presets[presetName]
		if !ok {
			return protocol.SpawnRequest{}, fmt.Errorf("--preset: unknown preset %q (available: %s)", presetName, availablePresets(pf))
		}
		if req.Model == "" && preset.Model != "" {
			req.Model = preset.Model
		}
		if len(preset.Labels) > 0 {
			req.Labels = mergeLabels(preset.Labels, req.Labels)
		}
	}

	// Tell the daemon to attach the reserved traceability auto-label
	// `fundi/resumed-from-session=<uuid>`.  Sent here rather than in req.Labels
	// because the daemon (correctly) rejects user-supplied labels in the
	// reserved fundi/ namespace.
	req.ResumedFromSession = info.SessionID

	return req, nil
}

// runResumeFromPiSession spawns a new child continuing any pi session, whether
// or not that session was ever managed by fundi.  It reads cwd, model, and
// thinking level from the session.jsonl and merges them with the usual sources
// (flags, RAFIKI_DEFAULT_MODEL, RAFIKI_DEFAULT_LABELS, preset).
//
// Priority for model:
//
//	--model flag > RAFIKI_DEFAULT_MODEL env var > jsonl model_change > error
//
// Priority for cwd:
//
//	--cwd flag > jsonl session header
//
// Priority for thinking:
//
//	--thinking flag > jsonl thinking_level_change > "" (pi default)
func runResumeFromPiSession(cmd *cobra.Command, input string) error {
	req, err := buildResumeFromPiSessionRequest(cmd, input)
	if err != nil {
		return err
	}

	// Send ctrl_spawn to the daemon.
	c := mustDial(cmd)
	defer c.Close()

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_spawn: %s", client.FormatError(resp))
	}

	var data protocol.SpawnResponseData
	_ = json.Unmarshal(resp.Data, &data)
	if err := setActive(data.ChildID); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	detached, _ := cmd.Flags().GetBool("detached")
	if detached {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	return attachAndDecide(cmd, data.ChildID, killOnExit, keepOnExit)
}
