package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newInstallExtensionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-extension",
		Short: "Install (or remove) the pic-helpers pi extension",
		Long: `Install the pic-helpers extension into ~/.pi/agent/extensions/pic-helpers/.

This extension registers pic-attach-aware slash commands (currently /reload) so
they work in daemon-managed pi children. It's harmless when used in native pi
sessions too — /reload there does the same thing as pi's built-in /reload.

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

	sourceDir, err := locatePicHelpersSource()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o700); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	// Wipe any previous installation if force.
	if force {
		_ = os.RemoveAll(destDir)
	}
	if err := copyDir(sourceDir, destDir); err != nil {
		return fmt.Errorf("copy extension: %w", err)
	}
	fmt.Fprintln(os.Stderr, "installed pic-helpers to", destDir)
	fmt.Fprintln(os.Stderr, "pi will auto-discover this extension on next run")
	return nil
}

// locatePicHelpersSource finds the bundled extensions/pic-helpers directory.
// Order: PIC_HELPERS_SOURCE env var, then sibling-of-pic-binary at
// <bindir>/../extensions/pic-helpers, then a few well-known dev paths.
func locatePicHelpersSource() (string, error) {
	if env := os.Getenv("PIC_HELPERS_SOURCE"); env != "" {
		return env, statDir(env)
	}
	self, err := os.Executable()
	if err == nil {
		// bin/pic -> ../extensions/pic-helpers
		sibling := filepath.Join(filepath.Dir(self), "..", "extensions", "pic-helpers")
		if abs, err := filepath.Abs(sibling); err == nil {
			if statDir(abs) == nil {
				return abs, nil
			}
		}
	}
	// Fallback: current working dir
	if abs, err := filepath.Abs("extensions/pic-helpers"); err == nil {
		if statDir(abs) == nil {
			return abs, nil
		}
	}
	return "", fmt.Errorf("could not find extensions/pic-helpers source dir; set PIC_HELPERS_SOURCE")
}

func statDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// copyDir copies src to dst recursively (small, no symlink handling, no
// permissions preservation beyond mode 0o644 for files / 0o755 for dirs).
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
