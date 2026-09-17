// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.graveland.dev/rafiki/pkg/profile"
)

// loadProfileAppendSystemPrompt reads profile.AppendSystemPromptFile(name): a
// plain-text appendix the operator edits directly instead of remembering
// --append-system-prompt on every `rafiki create`. A missing file means no
// addition, same as review.json's "missing file, zero value, no error" —
// there is nothing to seed here, since CoordinationPrompt already supplies
// the server-side default (gated on the MCP surface) for --kind claude, and
// this file applies uniformly to every kind.
func loadProfileAppendSystemPrompt(profileName string) (string, error) {
	path := profile.AppendSystemPromptFile(profileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// mergeAppendSystemPrompt joins the profile file's standing text with the
// --append-system-prompt flag's per-spawn text, file first — the same "the
// standing default reads before whatever the caller asked for" ordering
// CoordinationPrompt uses (pkg/claudeargv.WithCoordinationPrompt).
func mergeAppendSystemPrompt(fileText, flagText string) string {
	switch {
	case fileText == "":
		return flagText
	case flagText == "":
		return fileText
	default:
		return fileText + "\n\n" + flagText
	}
}
