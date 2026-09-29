// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// clipboardTimeout bounds every clipboard probe. A wedged pasteboard helper
// must cost the user a notice, not the cockpit.
const clipboardTimeout = 3 * time.Second

// clipboardPasteMsg is the outcome of one ^V: an image (mediaType + bytes), or
// text, or the reason there was neither. Exactly one of the three is set.
type clipboardPasteMsg struct {
	mediaType string
	data      []byte
	text      string
	err       error
}

// readClipboard is swappable so tests never touch a real pasteboard: a
// developer's clipboard holds whatever they copied last, which is the very
// definition of a flaky fixture.
var readClipboard = readClipboardImageOrText

// clipboardPasteCmd reads the clipboard off the UI goroutine. It never exits,
// never prints, and never blocks past clipboardTimeout.
func clipboardPasteCmd() tea.Cmd {
	return func() tea.Msg {
		mt, data, text, err := readClipboard()
		return clipboardPasteMsg{mediaType: mt, data: data, text: text, err: err}
	}
}

// runClipboardBytes runs a probe to completion and returns raw stdout — image
// probes emit bytes, so no newline trimming may touch them.
func runClipboardBytes(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runClipboardText is runClipboardBytes for text probes, trailing whitespace
// trimmed off.
func runClipboardText(name string, args ...string) (string, error) {
	out, err := runClipboardBytes(name, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// darwinPNGScript asks NSPasteboard for the clipboard's PNG, base64-encoded:
// AppleScript cannot carry raw bytes over stdout, and base64 survives every
// coercion. One script line per -e argument — AppleScript statements are line
// delimited. An image copied as TIFF (Finder's usual form) is not seen —
// reading it would be pointless anyway, since the upstream API wants
// png/jpeg/gif/webp.
var darwinPNGScript = []string{
	`use framework "Foundation"`,
	`set pb to current application's NSPasteboard's generalPasteboard()`,
	`set d to pb's dataForType(current application's NSPasteboardTypePNG)`,
	`if d is missing value then return ""`,
	`return ((d's base64EncodedStringWithOptions:0) as text)`,
}

// linuxImageProbes and linuxTextProbes cover Wayland and X11: whichever
// helper is installed answers, the other's absence is not an error.
var linuxImageProbes = []struct {
	name string
	args []string
}{
	{"wl-paste", []string{"--type", "image/png"}},
	{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
}

var linuxTextProbes = []struct {
	name string
	args []string
}{
	{"wl-paste", nil},
	{"xclip", []string{"-selection", "clipboard", "-o"}},
}

// readClipboardImageOrText is the real reader. Image first — that is what ^V
// is FOR — falling back to text, because a clipboard that holds text must
// still paste. A platform with neither probe errors; the notice says so.
func readClipboardImageOrText() (mediaType string, data []byte, text string, err error) {
	switch runtime.GOOS {
	case "darwin":
		if b64, e := runClipboardText("osascript", osascriptArgs(darwinPNGScript)...); e == nil && b64 != "" {
			if raw, dec := base64.StdEncoding.DecodeString(b64); dec == nil && len(raw) > 0 {
				return "image/png", raw, "", nil
			}
		}
		if text, e := runClipboardText("pbpaste"); e == nil && text != "" {
			return "", nil, text, nil
		}
		return "", nil, "", errors.New("clipboard holds no image or text")
	case "linux":
		for _, probe := range linuxImageProbes {
			if raw, e := runClipboardBytes(probe.name, probe.args...); e == nil && len(raw) > 0 {
				return "image/png", raw, "", nil
			}
		}
		for _, probe := range linuxTextProbes {
			if text, e := runClipboardText(probe.name, probe.args...); e == nil && text != "" {
				return "", nil, text, nil
			}
		}
		return "", nil, "", errors.New("clipboard holds no image or text (need wl-paste or xclip)")
	default:
		return "", nil, "", fmt.Errorf("clipboard read unsupported on %s", runtime.GOOS)
	}
}

// osascriptArgs renders script lines as one -e argument per line, the only
// shape osascript accepts for a multi-line program.
func osascriptArgs(lines []string) []string {
	args := make([]string, 0, 2*len(lines))
	for _, l := range lines {
		args = append(args, "-e", l)
	}
	return args
}
