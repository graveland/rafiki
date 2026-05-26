package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newSpawnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spawn [name]",
		Short: "Spawn a new pi child",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runSpawn,
	}
	cmd.Flags().String("cwd", "", "Working directory (required, must be absolute)")
	cmd.Flags().String("model", "", "Model (e.g. anthropic/claude-sonnet-4)")
	cmd.Flags().String("thinking", "", "Thinking level: off|minimal|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")

	_ = cmd.RegisterFlagCompletionFunc("cwd", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveFilterDirs
	})
	_ = cmd.RegisterFlagCompletionFunc("session", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"jsonl"}, cobra.ShellCompDirectiveFilterFileExt
	})
	_ = cmd.RegisterFlagCompletionFunc("fork", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"jsonl"}, cobra.ShellCompDirectiveFilterFileExt
	})
	_ = cmd.RegisterFlagCompletionFunc("thinking", cobra.FixedCompletions([]string{"off", "minimal", "low", "medium", "high", "xhigh"}, cobra.ShellCompDirectiveNoFileComp))

	return cmd
}

func runSpawn(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd == "" {
		return fmt.Errorf("--cwd is required")
	}
	if !strings.HasPrefix(cwd, "/") {
		return fmt.Errorf("--cwd must be absolute")
	}
	model, _ := cmd.Flags().GetString("model")
	thinking, _ := cmd.Flags().GetString("thinking")
	noSession, _ := cmd.Flags().GetBool("no-session")
	resume, _ := cmd.Flags().GetString("session")
	fork, _ := cmd.Flags().GetString("fork")
	noExt, _ := cmd.Flags().GetBool("no-extensions")
	exts, _ := cmd.Flags().GetStringSlice("extension")
	verbose, _ := cmd.Flags().GetBool("verbose")
	extraArgs, _ := cmd.Flags().GetStringSlice("extra-arg")

	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	req := protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Name:          name,
		Cwd:           cwd,
		Model:         model,
		Thinking:      thinking,
		NoSession:     noSession,
		ResumeSession: resume,
		ForkSession:   fork,
		NoExtensions:  noExt,
		Extensions:    exts,
		Verbose:       verbose,
		ExtraArgs:     extraArgs,
	}

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
		// Best effort — log to stderr but don't fail.
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}
