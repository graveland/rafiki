// SPDX-License-Identifier: Apache-2.0

package pymodules

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// RequirementsMarker is the exact comment line that introduces a
// requirements.txt-style dependency block inside a saved pymodule's code.
const RequirementsMarker = "# pymodule-requirements:"

// ParseRequirements extracts the requirements.txt-style lines a pymodule
// declares for itself. It looks for a line whose content, after trimming
// leading/trailing whitespace, is exactly RequirementsMarker. Every
// contiguous line after that marker which, after trimming trailing
// whitespace, starts with "#" is part of the block: strip the leading "#"
// and at most one following space, and keep what remains (which may be
// empty -- an empty result after stripping is dropped, it does not end the
// block). The first line that does NOT start with "#" ends the block.
// Returns nil if the marker is never found, or if the block after it is
// empty.
func ParseRequirements(code string) []string {
	lines := strings.Split(code, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != RequirementsMarker {
			continue
		}
		var reqs []string
		for _, l := range lines[i+1:] {
			l = strings.TrimRightFunc(l, unicode.IsSpace)
			if !strings.HasPrefix(l, "#") {
				break
			}
			l = strings.TrimPrefix(l, "#")
			l = strings.TrimPrefix(l, " ")
			if l == "" {
				continue
			}
			reqs = append(reqs, l)
		}
		return reqs
	}
	return nil
}

// RequirementsHash returns a stable hex digest of reqs (order-sensitive: the
// same lines in a different order hash differently, since a reordered
// requirements.txt is a real, if unusual, change worth rebuilding for). Used
// as the sole trigger for rebuilding a module's venv -- an unchanged hash
// means an unchanged venv. sha256 of strings.Join(reqs, "\n"), hex-encoded.
// An empty or nil reqs hashes the empty string, same as any other value --
// callers needing to distinguish "no requirements" from "hash of an empty
// block" do so by checking len(reqs) themselves, not by inspecting the hash.
func RequirementsHash(reqs []string) string {
	sum := sha256.Sum256([]byte(strings.Join(reqs, "\n")))
	return hex.EncodeToString(sum[:])
}
