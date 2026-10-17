// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/relsign"
	"go.graveland.dev/rafiki/pkg/version"
)

type upgradeOpts struct {
	Check   bool
	Version string // "" = latest
	Force   bool
}

type upgradeDeps struct {
	Out, Err     io.Writer
	Runner       runner
	Client       *releaseClient
	Keys         []relsign.Key
	GOOS, GOARCH string
	Exe          string // symlink-resolved path of the running binary
	Version      string // the running binary's version (version.String())
	Home         string
}

func newUpgradeCmd() *cobra.Command {
	var o upgradeOpts
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade rafiki to the latest release (self-managed installs)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			downloadBase := os.Getenv("RAFIKI_DOWNLOAD_BASE")
			if downloadBase != "" && o.Version == "" {
				return errors.New("RAFIKI_DOWNLOAD_BASE requires --version")
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(exe)
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			return runUpgrade(cmd.Context(), upgradeDeps{
				Out:     cmd.OutOrStdout(),
				Err:     cmd.ErrOrStderr(),
				Runner:  execRunner{},
				Client:  newReleaseClient(downloadBase),
				Keys:    relsign.TrustedKeys,
				GOOS:    runtime.GOOS,
				GOARCH:  runtime.GOARCH,
				Exe:     resolved,
				Version: version.String(),
				Home:    home,
			}, o)
		},
	}
	cmd.Flags().BoolVar(&o.Check, "check", false, "report whether an update is available without installing it")
	cmd.Flags().StringVar(&o.Version, "version", "", "install this exact tag (vX.Y.Z), including an older one")
	cmd.Flags().BoolVar(&o.Force, "force", false, "install even when Homebrew- or source-managed, or already up to date")
	return cmd
}

func runUpgrade(ctx context.Context, d upgradeDeps, o upgradeOpts) error {
	channel := installstate.DetectChannel(d.Exe, d.Version)

	tag := o.Version
	if tag != "" {
		if _, err := relsign.ParseTag(tag); err != nil {
			return err
		}
	}

	if o.Check {
		if tag == "" {
			latest, err := d.Client.latestTag(ctx)
			if err != nil {
				return err
			}
			tag = latest
		}
		fmt.Fprintf(d.Out, "running: %s\n", d.Version)
		fmt.Fprintf(d.Out, "latest:  %s\n", tag)
		switch channel {
		case installstate.ChannelBrew:
			fmt.Fprintln(d.Out, "installed by Homebrew: run brew upgrade rafiki")
		case installstate.ChannelSource:
			fmt.Fprintln(d.Out, "built from source: rebuild to update")
		default:
			cmp, err := relsign.CompareTags(tag, d.Version)
			if err != nil {
				return err
			}
			if cmp <= 0 {
				fmt.Fprintln(d.Out, "up to date")
			} else {
				fmt.Fprintln(d.Out, "update available: run rafiki upgrade")
			}
		}
		return nil
	}

	switch channel {
	case installstate.ChannelBrew:
		if !o.Force {
			return errors.New("installed by Homebrew; run: brew upgrade rafiki (or pass --force to install a self-managed copy in ~/.local/bin)")
		}
	case installstate.ChannelSource:
		if !o.Force {
			return errors.New("built from source; rebuild to update (or pass --force to install the release into ~/.local/bin)")
		}
	}

	if tag == "" {
		latest, err := d.Client.latestTag(ctx)
		if err != nil {
			return err
		}
		tag = latest
	}

	if channel == installstate.ChannelTarball {
		cmp, err := relsign.CompareTags(tag, d.Version)
		if err != nil {
			return err
		}
		if o.Version == "" && cmp <= 0 && !o.Force {
			fmt.Fprintf(d.Out, "already up to date (%s)\n", d.Version)
			return nil
		}
		if o.Version != "" && tag == d.Version && !o.Force {
			fmt.Fprintf(d.Out, "already at %s\n", tag)
			return nil
		}
	}

	dest := filepath.Dir(d.Exe)
	if channel == installstate.ChannelTarball {
		fmt.Fprintf(d.Out, "upgrading %s -> %s in %s\n", d.Version, tag, dest)
	} else {
		dest = filepath.Join(d.Home, ".local", "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "installing %s into %s\n", tag, dest)
	}

	fmt.Fprintf(d.Out, "downloading and verifying %s\n", tag)
	staged, err := d.Client.stageRelease(ctx, d.Keys, tag, d.GOOS, d.GOARCH, dest)
	if err != nil {
		return err
	}
	defer staged.cleanup()

	if err := smokeCheck(ctx, d.Runner, staged.Rafiki, tag); err != nil {
		return err
	}

	if err := swapBinaries([]swapPair{
		{staged.Rafikid, filepath.Join(dest, "rafikid")},
		{staged.Rafiki, filepath.Join(dest, "rafiki")},
	}); err != nil {
		return err
	}

	if channel != installstate.ChannelTarball {
		st, _, err := installstate.Load()
		if err != nil {
			return fmt.Errorf("installed %s, but could not record the channel: %w", tag, err)
		}
		st.Channel = installstate.ChannelTarball
		if err := installstate.Save(st); err != nil {
			return fmt.Errorf("installed %s, but could not record the channel: %w", tag, err)
		}
	}

	fmt.Fprintln(d.Out, "restarting services")
	stdout, stderr, err := d.Runner.Run(ctx, nil, filepath.Join(dest, "rafiki"), "service", "refresh")
	fmt.Fprint(d.Out, string(stdout))
	fmt.Fprint(d.Err, string(stderr))
	if err != nil {
		return fmt.Errorf("upgraded to %s, but service refresh failed: %w\nrun: %s service refresh", tag, err, filepath.Join(dest, "rafiki"))
	}

	if channel != installstate.ChannelTarball {
		if p, err := d.Runner.LookPath("rafiki"); err == nil && p != filepath.Join(dest, "rafiki") {
			fmt.Fprintf(d.Err, "warning: 'rafiki' on PATH resolves to %s, not %s\n", p, filepath.Join(dest, "rafiki"))
		}
		if channel == installstate.ChannelBrew {
			fmt.Fprintln(d.Out, "Homebrew still has a copy. Remove it with: brew uninstall rafiki")
		}
	}

	fmt.Fprintf(d.Out, "upgraded to %s\n", tag)
	return nil
}
