package main

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs [id|name]",
		Short: "Show the on-disk log location for a child",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runLogs,
	}
	cmd.Flags().Bool("cat", false, "Print the contents of out.jsonl.gz to stdout")
	cmd.Flags().Bool("in", false, "Cat in.jsonl.gz instead of out.jsonl.gz (implies --cat)")
	cmd.Flags().Bool("err", false, "Cat err.log.gz instead of out.jsonl.gz (implies --cat)")

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

func runLogs(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTarget(cmdCtx(cmd), c, target)
	if err != nil {
		return err
	}

	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".pi", "run", "logs", childID)

	if _, err := os.Stat(logsDir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no logs at %s (child still alive, or persistence mode is `never`)", logsDir)
	}

	wantIn, _ := cmd.Flags().GetBool("in")
	wantErr, _ := cmd.Flags().GetBool("err")
	wantCat, _ := cmd.Flags().GetBool("cat")

	// --in / --err imply --cat
	if wantIn || wantErr {
		wantCat = true
	}

	if !wantCat {
		fmt.Println(logsDir)
		return nil
	}

	file := "out.jsonl.gz"
	if wantIn {
		file = "in.jsonl.gz"
	}
	if wantErr {
		file = "err.log.gz"
	}

	path := filepath.Join(logsDir, file)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	_, err = io.Copy(os.Stdout, gz)
	return err
}
