package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/cmd/pic/picembed"
)

func newInstallExtensionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-extension",
		Short: "Install (or remove) the pic-helpers pi extension",
		Long: `Install the pic-helpers extension into ~/.pi/agent/extensions/pic-helpers/.

This extension registers pic-attach-aware slash commands (currently /reload) so
they work in daemon-managed pi children. It's harmless in native pi sessions too —
/reload there does the same thing as pi's built-in /reload.

The extension is bundled into the pic binary; no source tree required.

Use --remove to uninstall.`,
		Args: cobra.NoArgs,
		RunE: runInstallExtension,
	}
	cmd.Flags().Bool("force", false, "Overwrite existing installation")
	cmd.Flags().Bool("remove", false, "Uninstall (remove the installed extension directory)")
	cmd.MarkFlagsMutuallyExclusive("force", "remove")
	return cmd
}

func runInstallExtension(cmd *cobra.Command, _ []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	destDir := filepath.Join(home, ".pi", "agent", "extensions", "pic-helpers")

	remove, _ := cmd.Flags().GetBool("remove")
	if remove {
		if _, err := os.Stat(destDir); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "pic-helpers is not installed at", destDir)
			return nil
		}
		if err := os.RemoveAll(destDir); err != nil {
			return fmt.Errorf("remove %s: %w", destDir, err)
		}
		fmt.Fprintln(os.Stderr, "removed", destDir)
		return nil
	}

	force, _ := cmd.Flags().GetBool("force")
	if _, err := os.Stat(destDir); err == nil && !force {
		return fmt.Errorf("already installed at %s (use --force to overwrite)", destDir)
	}
	if force {
		_ = os.RemoveAll(destDir)
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o700); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	if err := installFromEmbed(destDir); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintln(os.Stderr, "installed pic-helpers to", destDir)
	fmt.Fprintln(os.Stderr, "pi will auto-discover this extension on next run")
	return nil
}

// installFromEmbed walks the embedded pic-helpers tree and writes every file
// into destDir, preserving the relative structure.
//
// The embed.FS roots all entries at "pic-helpers/..." — strip that prefix
// when computing the destination path.
func installFromEmbed(destDir string) error {
	return fs.WalkDir(picembed.PicHelpers, "pic-helpers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("pic-helpers", path)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return writeEmbedded(path, target)
	})
}

func writeEmbedded(srcPath, dstPath string) error {
	src, err := picembed.PicHelpers.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}
