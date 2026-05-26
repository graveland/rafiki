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
		Use:   "logs <id|name>",
		Short: "Show the captured logs for a child",
		Long: `Show the on-disk logs for a child (controller-captured stdin/stdout/stderr).

By default, prints the contents of out.jsonl.gz (events from the pi child).
Use --in / --err to see other streams; --all for everything; --path to
print just the directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runLogs,
	}
	cmd.Flags().Bool("in", false, "Print in.jsonl.gz (commands sent to the child) instead")
	cmd.Flags().Bool("err", false, "Print err.log.gz (stderr) instead")
	cmd.Flags().Bool("all", false, "Print all three streams with separator headers")
	cmd.Flags().Bool("path", false, "Print just the log directory path")

	cmd.MarkFlagsMutuallyExclusive("in", "err", "all", "path")

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

	wantPath, _ := cmd.Flags().GetBool("path")
	if wantPath {
		fmt.Println(logsDir)
		return nil
	}

	wantIn, _ := cmd.Flags().GetBool("in")
	wantErr, _ := cmd.Flags().GetBool("err")
	wantAll, _ := cmd.Flags().GetBool("all")

	if wantAll {
		for _, name := range []string{"in.jsonl.gz", "out.jsonl.gz", "err.log.gz"} {
			fmt.Printf("=== %s ===\n", name)
			if err := zcatTo(os.Stdout, filepath.Join(logsDir, name)); err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
			}
		}
		return nil
	}

	file := "out.jsonl.gz" // default
	if wantIn {
		file = "in.jsonl.gz"
	}
	if wantErr {
		file = "err.log.gz"
	}
	return zcatTo(os.Stdout, filepath.Join(logsDir, file))
}

func zcatTo(w io.Writer, path string) error {
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
	_, err = io.Copy(w, gz)
	return err
}
