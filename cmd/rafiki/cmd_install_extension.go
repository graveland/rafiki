package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/cmd/rafiki/helpersembed"
	"go.graveland.dev/rafiki/pkg/paths"
)

// piExtensionsDir is pi's own extensions directory. Deliberately not resolved
// through internal/paths: that package covers what rafiki owns, and this is pi's
// contract — writing extensions here is how pi discovers them.
func piExtensionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".pi", "agent", "extensions"), nil
}

// helpersDestDir is where the bundled extension installs to.
func helpersDestDir() (string, error) {
	dir, err := piExtensionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, helpersembed.Dir), nil
}

func newInstallExtensionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-extension",
		Short: "Install (or update) the rafiki-helpers pi extension",
		Long: `Install or update the rafiki-helpers extension at
~/.pi/agent/extensions/rafiki-helpers/.

rafiki create also runs this automatically (use --no-install-helpers there to
skip). Running explicitly is useful if you want it installed without
spawning a child.

If rafiki-helpers is already installed at the bundled version, this is a
no-op (use --force to reinstall anyway). Use --remove to uninstall.`,
		Args: cobra.NoArgs,
		RunE: runInstallExtension,
	}
	cmd.Flags().Bool("force", false, "Overwrite existing installation")
	cmd.Flags().Bool("remove", false, "Uninstall (remove the installed extension directory)")
	cmd.MarkFlagsMutuallyExclusive("force", "remove")
	return cmd
}

func runInstallExtension(cmd *cobra.Command, _ []string) error {
	destDir, err := helpersDestDir()
	if err != nil {
		return err
	}

	remove, _ := cmd.Flags().GetBool("remove")
	if remove {
		if _, err := os.Stat(destDir); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s is not installed at %s\n", helpersembed.Dir, destDir)
			return nil
		}
		if err := os.RemoveAll(destDir); err != nil {
			return fmt.Errorf("remove %s: %w", destDir, err)
		}
		fmt.Fprintln(os.Stderr, "removed", destDir)
		return nil
	}

	bundled, installed, err := helpersVersionCheck()
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool("force")
	if installed != "" && installed == bundled && !force {
		fmt.Fprintf(os.Stderr, "%s is up to date (version %s) at %s\n", helpersembed.Dir, installed, destDir)

		return nil
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o700); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	_ = os.RemoveAll(destDir)
	if err := installFromEmbed(destDir); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	switch {
	case installed == "":
		fmt.Fprintf(os.Stderr, "installed %s to %s\n", helpersembed.Dir, destDir)
	case installed != bundled:
		fmt.Fprintf(os.Stderr, "updated %s %s \u2192 %s at %s\n", helpersembed.Dir, installed, bundled, destDir)
	default:
		fmt.Fprintf(os.Stderr, "reinstalled %s (version %s) at %s\n", helpersembed.Dir, bundled, destDir)
	}
	fmt.Fprintln(os.Stderr, "pi will auto-discover this extension on next run")
	return nil
}

// ensureHelpersInstalled installs or updates rafiki-helpers to match the
// version bundled in this rafiki binary. Silent on success. Returns nil if
// skipped due to opt-out env var.
//
// If install fails for any reason (permissions, disk full, etc.), the
// caller should log a warning and continue — rafiki-helpers is a nice-to-have.
func ensureHelpersInstalled() error {
	if paths.Get(paths.NoAutoInstallHelpers) != "" {
		return nil
	}

	destDir, err := helpersDestDir()
	if err != nil {
		return err
	}

	bundled, err := readBundledHelpersVersion()
	if err != nil {
		return fmt.Errorf("read bundled version: %w", err)
	}

	installed, err := readInstalledHelpersVersion(destDir)
	switch {
	case err == nil && installed == bundled:
		return nil // up to date — stay silent, this runs on every create
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read installed version: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o700); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	_ = os.RemoveAll(destDir)
	if err := installFromEmbed(destDir); err != nil {
		return err
	}
	return nil
}

// helpersVersionCheck returns (bundled, installed, error). If rafiki-helpers
// is not installed, installed is "" and error is nil. Used by both
// auto-install and the explicit install-extension command.
func helpersVersionCheck() (bundled, installed string, err error) {
	destDir, derr := helpersDestDir()
	if derr != nil {
		return "", "", derr
	}

	bundled, err = readBundledHelpersVersion()
	if err != nil {
		return "", "", err
	}

	installed, ierr := readInstalledHelpersVersion(destDir)
	if ierr != nil && !errors.Is(ierr, fs.ErrNotExist) {
		return bundled, "", ierr
	}
	return bundled, installed, nil
}

func readBundledHelpersVersion() (string, error) {
	f, err := helpersembed.Helpers.Open(helpersembed.Dir + "/package.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return extractPkgJSONVersion(f)
}

func readInstalledHelpersVersion(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return extractPkgJSONVersion(f)
}

func extractPkgJSONVersion(r io.Reader) (string, error) {
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r).Decode(&pkg); err != nil {
		return "", err
	}
	if pkg.Version == "" {
		return "", fmt.Errorf("package.json has no version field")
	}
	return pkg.Version, nil
}

// installFromEmbed walks the embedded extension tree and writes every file
// into destDir, preserving the relative structure.
//
// The embed.FS roots all entries at "<Dir>/..." — strip that prefix when
// computing the destination path.
func installFromEmbed(destDir string) error {
	return fs.WalkDir(helpersembed.Helpers, helpersembed.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(helpersembed.Dir, path)
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
	src, err := helpersembed.Helpers.Open(srcPath)
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
