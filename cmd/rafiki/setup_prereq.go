// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Detail strings a prereq's Present function returns to steer checkPrereqs. A
// present prereq returns ("", ok=true); an absent one names *why* so the
// right remedy is offered (start the daemon vs. install the tool vs. log in).
const (
	detailNotInstalled     = "not installed"
	detailDaemonDown       = "daemon not running"
	detailLoggedOut        = "logged out"
	detailDockerPermission = "permission denied"
)

// prereq is one external tool `rafiki setup` checks for and can install.
type prereq struct {
	Name     string
	Required bool
	Present  func(ctx context.Context, r runner) (ok bool, detail string, err error)
	Install  func(goos string, hasBrew bool) [][]string // offer list; nil = no automatic install
}

// errDockerGroup is the stop message after adding the user to the docker group:
// the new group membership is not in the current login session.
var errDockerGroup = errors.New("log out and back in so your user can reach docker, then re-run rafiki setup")

// homebrewInstallOneLiner is printed when a darwin host has no brew but needs
// one to install a required tool.
const homebrewInstallOneLiner = `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`

// prereqs is the ordered list of tools setup checks. Required tools block the
// install when missing; the optional ones are offered through askMulti.
func prereqs() []prereq {
	return []prereq{
		{
			Name:     "docker",
			Required: true,
			Present: func(ctx context.Context, r runner) (bool, string, error) {
				// Probe with plain `docker info`: --format would replace the real
				// failure (e.g. "permission denied ...") with a Go template error,
				// which names no cause. Classify from the raw output instead.
				stdout, stderr, err := r.Run(ctx, nil, "docker", "info")
				if err != nil {
					// Not being in the docker group reads as a dead daemon; it is
					// a permission problem with a different remedy (add the user
					// to the group, then re-login). Prefer that diagnosis.
					combined := strings.ToLower(string(stdout) + string(stderr))
					if strings.Contains(combined, detailDockerPermission) {
						return false, detailDockerPermission, nil
					}
					// The binary may be installed with the daemon merely stopped.
					if _, lerr := r.LookPath("docker"); lerr == nil {
						return false, detailDaemonDown, nil
					}
					return false, detailNotInstalled, nil
				}
				return true, "", nil
			},
			Install: func(goos string, hasBrew bool) [][]string {
				switch goos {
				case "darwin":
					return [][]string{{"brew", "install", "--cask", "orbstack"}}
				case "linux":
					return [][]string{
						{"sudo", "apt-get", "install", "-y", "docker.io"},
						{"sudo", "usermod", "-aG", "docker", os.Getenv("USER")},
					}
				}
				return nil
			},
		},
		{
			Name:     "claude",
			Required: true,
			Present: func(ctx context.Context, r runner) (bool, string, error) {
				out, _, err := r.Run(ctx, nil, "claude", "auth", "status", "--json")
				if err != nil {
					if _, lerr := r.LookPath("claude"); lerr != nil {
						return false, detailNotInstalled, nil
					}
					return false, detailLoggedOut, nil
				}
				var status struct {
					LoggedIn bool `json:"loggedIn"`
				}
				if err := json.Unmarshal(out, &status); err != nil {
					return false, "", fmt.Errorf("claude auth status: %w", err)
				}
				if !status.LoggedIn {
					return false, detailLoggedOut, nil
				}
				return true, "", nil
			},
			Install: func(goos string, hasBrew bool) [][]string {
				return [][]string{{"bash", "-c", "curl -fsSL https://claude.ai/install.sh | bash"}}
			},
		},
		{
			Name:     "rg",
			Required: true,
			Present: func(ctx context.Context, r runner) (bool, string, error) {
				if _, err := r.LookPath("rg"); err != nil {
					return false, detailNotInstalled, nil
				}
				return true, "", nil
			},
			Install: func(goos string, hasBrew bool) [][]string {
				switch goos {
				case "darwin":
					return [][]string{{"brew", "install", "ripgrep"}}
				case "linux":
					return [][]string{{"sudo", "apt-get", "install", "-y", "ripgrep"}}
				}
				return nil
			},
		},
		{
			Name: "uv",
			Present: func(ctx context.Context, r runner) (bool, string, error) {
				if _, err := r.LookPath("uv"); err != nil {
					return false, detailNotInstalled, nil
				}
				return true, "", nil
			},
			Install: func(goos string, hasBrew bool) [][]string {
				switch goos {
				case "darwin":
					return [][]string{{"brew", "install", "uv"}}
				case "linux":
					return [][]string{{"bash", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh"}}
				}
				return nil
			},
		},
		{
			Name: "ollama",
			Present: func(ctx context.Context, r runner) (bool, string, error) {
				if _, err := r.LookPath("ollama"); err != nil {
					return false, detailNotInstalled, nil
				}
				return true, "", nil
			},
			Install: func(goos string, hasBrew bool) [][]string {
				switch goos {
				case "darwin":
					return [][]string{{"brew", "install", "ollama"}}
				case "linux":
					return [][]string{{"bash", "-c", "curl -fsSL https://ollama.com/install.sh | sh"}}
				}
				return nil
			},
		},
		{
			Name: "rtk",
			Present: func(ctx context.Context, r runner) (bool, string, error) {
				if _, err := r.LookPath("rtk"); err != nil {
					return false, detailNotInstalled, nil
				}
				return true, "", nil
			},
			Install: func(goos string, hasBrew bool) [][]string {
				if goos == "darwin" && hasBrew {
					return [][]string{{"brew", "install", "rtk"}}
				}
				// No apt package: rtk's README is the only install path on linux.
				return nil
			},
		},
	}
}

// optionalPrereqNames is the ordered option list askMulti presents and the
// values it returns.
func optionalPrereqNames() []string {
	return []string{"uv", "ollama", "rtk"}
}

// detectOptionalPrereqs returns the optional tools already installed, so a
// first run preselects what the machine has instead of offering to install it.
func detectOptionalPrereqs(ctx context.Context, r runner) ([]string, error) {
	var found []string
	for _, p := range prereqs() {
		if p.Required || !containsString(optionalPrereqNames(), p.Name) {
			continue
		}
		ok, _, err := p.Present(ctx, r)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, p.Name)
		}
	}
	return found, nil
}

// checkPrereqs verifies every required tool is present — and every chosen
// optional tool — installing what it can, and returns the optional tools the
// user chose (to be saved to install-state).
func checkPrereqs(ctx context.Context, r runner, w *wizard, goos string, optional []string) ([]string, error) {
	hasBrew := false
	if _, err := r.LookPath("brew"); err == nil {
		hasBrew = true
	}
	detected, err := detectOptionalPrereqs(ctx, r)
	if err != nil {
		return nil, err
	}
	chosen, err := w.askMulti("Optional tools", "with", optionalPrereqNames(), optional, detected)
	if err != nil {
		return nil, err
	}

	for _, p := range prereqs() {
		if !p.Required && !containsString(chosen, p.Name) {
			continue
		}
		ok, detail, err := p.Present(ctx, r)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}

		switch {
		case p.Name == "docker":
			if err := installDocker(ctx, r, w, goos, hasBrew, detail); err != nil {
				return nil, err
			}
		case p.Name == "claude" && detail == detailLoggedOut:
			if err := runInstallCommand(ctx, r, w, p.Name, []string{"claude", "auth", "login"}, goos, hasBrew); err != nil {
				return nil, err
			}
		default:
			cmds := p.Install(goos, hasBrew)
			if len(cmds) == 0 {
				if p.Required {
					return nil, fmt.Errorf("%s is required; install it manually for %s", p.Name, goos)
				}
				fmt.Fprintf(w.out, "note: %s has no automatic installer on %s; see its README\n", p.Name, goos)
				continue
			}
			for _, argv := range cmds {
				if err := runInstallCommand(ctx, r, w, p.Name, argv, goos, hasBrew); err != nil {
					return nil, err
				}
			}
		}
	}
	return chosen, nil
}

// installDocker offers the platform's docker install. On linux the daemon and
// group are set up but the user must re-login, so it stops with a message.
func installDocker(ctx context.Context, r runner, w *wizard, goos string, hasBrew bool, detail string) error {
	if detail == detailDockerPermission {
		if goos == "darwin" {
			return errors.New("your user cannot reach the Docker daemon socket; check Docker/OrbStack's permission settings")
		}
		return errors.New("docker is installed but your user cannot reach the Docker daemon socket; run: sudo usermod -aG docker $USER — then log out and back in and re-run rafiki setup")
	}
	if detail == detailDaemonDown {
		if goos == "darwin" {
			return errors.New("docker is installed but the daemon is not running; start it with: open -a OrbStack")
		}
		return errors.New("docker is installed but the daemon is not running; start it with: sudo systemctl start docker")
	}
	switch goos {
	case "darwin":
		return runInstallCommand(ctx, r, w, "docker", []string{"brew", "install", "--cask", "orbstack"}, goos, hasBrew)
	case "linux":
		for _, argv := range [][]string{
			{"sudo", "apt-get", "install", "-y", "docker.io"},
			{"sudo", "usermod", "-aG", "docker", os.Getenv("USER")},
		} {
			if err := runInstallCommand(ctx, r, w, "docker", argv, goos, hasBrew); err != nil {
				return err
			}
		}
		return errDockerGroup
	}
	return fmt.Errorf("docker is required; install it manually for %s", goos)
}

// runInstallCommand offers one install command through confirmRun and then runs
// it attached to the terminal (installers prompt for sudo/brew). A decline or a
// failed command stops with a message naming the exact command.
func runInstallCommand(ctx context.Context, r runner, w *wizard, name string, argv []string, goos string, hasBrew bool) error {
	// A brew-gated install on a Mac without Homebrew cannot proceed; stop with
	// the one-liner rather than offer a command that will fail. Non-brew
	// installers (e.g. the claude curl script) still work.
	if goos == "darwin" && !hasBrew && len(argv) > 0 && argv[0] == "brew" {
		return fmt.Errorf("homebrew is required to install missing tools on macOS; install it with:\n  %s", homebrewInstallOneLiner)
	}
	ok, err := w.confirmRun(argv)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is required; install it with: %s", name, shellJoin(argv))
	}
	if err := r.RunInteractive(ctx, argv...); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return fmt.Errorf("%s is required; the install failed: %s: %s", name, shellJoin(argv), strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("%s is required; install it with: %s (install failed: %v)", name, shellJoin(argv), err)
	}
	return nil
}

// shellJoin renders argv the way a shell would spell it, for a copy-pasteable
// "install it with:" hint. An element is quoted only when it would otherwise be
// split or expanded.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'`$&|;<>()*?[]{}!\\") {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
