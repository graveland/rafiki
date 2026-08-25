// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B4 retired --kind pi. These are the references it left behind; each one is
// either dead code or, in limits.go's case, a user-facing message that is now
// false — it tells the caller pi is "the default when kind is omitted", which
// has not been true since B4's last commit.
func TestNoPiReferencesRemain(t *testing.T) {
	roots := []string{"../../cmd", "../../pkg"}
	var offenders []string
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			if strings.HasSuffix(path, "no_pi_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(b), "KindPi") {
				offenders = append(offenders, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("KindPi still referenced in: %s", strings.Join(offenders, ", "))
	}
}
