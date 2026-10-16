// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// wizard holds what every setup prompt needs to resolve one answer.
//
// The rule for every prompt: it has a flag, a saved answer, and a detected
// default, and the precedence is flag (if Changed) > saved > detected.
// Interactive mode shows a form prefilled with that value; --non-interactive
// (or any non-terminal stdin) skips the form and takes the value, and a
// required value still empty there is an error naming the flag.
type wizard struct {
	cmd            *cobra.Command
	nonInteractive bool      // --non-interactive: never show a form
	in             io.Reader // os.Stdin in production
	out            io.Writer // os.Stdout in production

	// interactive is computed by newWizard: not --non-interactive and stdin is
	// a terminal. When false, no form is ever shown and answers are taken as-is.
	interactive bool
}

// newWizard reads the "non-interactive" bool flag from cmd. The caller
// registers it; a missing flag simply reads as false. It decides interactivity
// once, here, so a form is never shown under --non-interactive or a piped
// stdin.
func newWizard(cmd *cobra.Command) *wizard {
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
	return &wizard{
		cmd:            cmd,
		nonInteractive: nonInteractive,
		in:             os.Stdin,
		out:            os.Stdout,
		interactive:    !nonInteractive && term.IsTerminal(int(os.Stdin.Fd())),
	}
}

// resolveString returns flag (if cmd.Flags().Changed(flag)) > saved > detected.
func (w *wizard) resolveString(flag, saved, detected string) string {
	if w.cmd.Flags().Changed(flag) {
		v, _ := w.cmd.Flags().GetString(flag)
		return v
	}
	if saved != "" {
		return saved
	}
	return detected
}

// askString shows an huh Input titled title, prefilled with resolveString's
// value, when interactive; otherwise it returns that value. A required value
// that is still empty under non-interactive mode is an error naming the flag.
func (w *wizard) askString(title, flag, saved, detected string, required bool) (string, error) {
	v := w.resolveString(flag, saved, detected)
	if !w.interactive {
		if required && v == "" {
			return "", missingFlagError(flag)
		}
		return v, nil
	}

	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).Value(&v),
	)).WithInput(w.in).WithOutput(w.out)
	if err := form.Run(); err != nil {
		return "", err
	}
	if required && v == "" {
		return "", missingFlagError(flag)
	}
	return v, nil
}

// askSecret is askString with the value masked (password echo mode), for
// prompts whose value is a secret: a DSN or an API key. Precedence is
// identical to askString: flag Changed > saved > detected, and a required
// empty value under non-interactive mode is an error naming the flag.
func (w *wizard) askSecret(title, flag, saved, detected string, required bool) (string, error) {
	v := w.resolveString(flag, saved, detected)
	if !w.interactive {
		if required && v == "" {
			return "", missingFlagError(flag)
		}
		return v, nil
	}

	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).Value(&v).EchoMode(huh.EchoModePassword),
	)).WithInput(w.in).WithOutput(w.out)
	if err := form.Run(); err != nil {
		return "", err
	}
	if required && v == "" {
		return "", missingFlagError(flag)
	}
	return v, nil
}

// askConfirm resolves a bool with the same precedence; saved is nil when the
// answer has never been asked for.
func (w *wizard) askConfirm(title, flag string, saved *bool, detected bool) (bool, error) {
	var v bool
	switch {
	case w.cmd.Flags().Changed(flag):
		v, _ = w.cmd.Flags().GetBool(flag)
	case saved != nil:
		v = *saved
	default:
		v = detected
	}

	if !w.interactive {
		return v, nil
	}

	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(title).Value(&v),
	)).WithInput(w.in).WithOutput(w.out)
	if err := form.Run(); err != nil {
		return false, err
	}
	return v, nil
}

// askMulti shows a huh MultiSelect over options, preselected from flag
// (StringSlice if Changed) > saved (non-nil) > detected. The returned values
// are in options' order, so a caller indexes them positionally.
func (w *wizard) askMulti(title, flag string, options, saved, detected []string) ([]string, error) {
	var preselected []string
	switch {
	case w.cmd.Flags().Changed(flag):
		preselected, _ = w.cmd.Flags().GetStringSlice(flag)
	case saved != nil:
		preselected = saved
	default:
		preselected = detected
	}

	selected := make([]string, 0, len(options))
	for _, opt := range options {
		if containsString(preselected, opt) {
			selected = append(selected, opt)
		}
	}

	if !w.interactive {
		return selected, nil
	}

	opts := make([]huh.Option[string], 0, len(options))
	for _, opt := range options {
		opts = append(opts, huh.NewOption(opt, opt).Selected(containsString(preselected, opt)))
	}
	chosen := []string{}
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title(title).Options(opts...).Value(&chosen),
	)).WithInput(w.in).WithOutput(w.out)
	if err := form.Run(); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(options))
	for _, opt := range options {
		if containsString(chosen, opt) {
			out = append(out, opt)
		}
	}
	return out, nil
}

// confirmRun prints the exact command line and asks "Run it?". Non-interactive
// mode returns true only when --non-interactive was given.
func (w *wizard) confirmRun(argv []string) (bool, error) {
	fmt.Fprintln(w.out, strings.Join(argv, " "))
	if !w.interactive {
		return w.nonInteractive, nil
	}

	v := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Run it?").Value(&v),
	)).WithInput(w.in).WithOutput(w.out)
	if err := form.Run(); err != nil {
		return false, err
	}
	return v, nil
}

func missingFlagError(flag string) error {
	return fmt.Errorf("missing --%s (required with --non-interactive or without a terminal)", flag)
}

// errNoTerminal is returned by requireTerminalOrNonInteractive.
var errNoTerminal = errors.New("no terminal to ask questions on; pass --non-interactive to use flags, saved answers and defaults")

// requireTerminalOrNonInteractive refuses a setup run that has no terminal to
// prompt on unless --non-interactive was given explicitly, so nothing (an
// agent, a pipe) silently takes every default. It lives at the setup
// commands' entry, not in newWizard: rafiki create builds wizards too and
// must keep working piped.
func requireTerminalOrNonInteractive(cmd *cobra.Command, stdinIsTerminal bool) error {
	if stdinIsTerminal {
		return nil
	}
	if ni, _ := cmd.Flags().GetBool("non-interactive"); ni {
		return nil
	}
	return errNoTerminal
}

// choice is one askSelect option: Value is returned, Label is shown.
type choice struct{ Value, Label string }

// askSelect resolves one of choices' Values: flag (GetString, if Changed) >
// saved (when it is one of the Values) > detected. Interactive mode shows an
// huh Select preselected on that value; non-interactive returns it.
func (w *wizard) askSelect(title, flag string, choices []choice, saved, detected string) (string, error) {
	v := detected
	switch {
	case w.cmd.Flags().Changed(flag):
		v, _ = w.cmd.Flags().GetString(flag)
	case saved != "" && choiceHas(choices, saved):
		v = saved
	}

	if !w.interactive {
		return v, nil
	}

	opts := make([]huh.Option[string], 0, len(choices))
	for _, c := range choices {
		opts = append(opts, huh.NewOption(c.Label, c.Value))
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title(title).Options(opts...).Value(&v),
	)).WithInput(w.in).WithOutput(w.out)
	if err := form.Run(); err != nil {
		return "", err
	}
	return v, nil
}

// choiceHas reports whether value is one of choices' Values.
func choiceHas(choices []choice, value string) bool {
	for _, c := range choices {
		if c.Value == value {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// waitForSocket returns once path exists as a unix socket that accepts a
// connection, or ctx is done. It watches filepath.Dir(path) with fsnotify
// (creating the directory 0700 if absent so the watch can be placed), checks
// once after the watch is armed (so a socket created before the watch is not
// missed), and re-checks on each Create event for path. A dial that fails
// right after the Create (bind before listen) is retried on the next event
// only.
func waitForSocket(ctx context.Context, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		return err
	}

	// Check once after arming: a socket created before the watch was placed
	// emits no Create event we can see.
	if socketReady(path) {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-watcher.Events:
			if !ok {
				return errors.New("fsnotify watcher closed")
			}
			if ev.Op&fsnotify.Create != 0 && filepath.Clean(ev.Name) == filepath.Clean(path) {
				if socketReady(path) {
					return nil
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return errors.New("fsnotify watcher closed")
			}
			return err
		}
	}
}

// socketReady reports whether path is a unix socket that accepts a dial.
func socketReady(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
