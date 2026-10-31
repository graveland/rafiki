// SPDX-License-Identifier: Apache-2.0

package msg_test

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// sdkImporters is the closed set of packages allowed to import the Anthropic
// SDK directly. It may only shrink. pkg/msg/anthropic, the Anthropic codec, is
// the only package that touches the SDK.
var sdkImporters = []string{
	"go.graveland.dev/rafiki/pkg/msg/anthropic",
}

// TestAnthropicSDKImportRatchet asserts the tree's direct Anthropic SDK
// importers are exactly sdkImporters. It FAILS, never skips, when go is not on
// PATH: a ratchet that silently skips is not a ratchet.
func TestAnthropicSDKImportRatchet(t *testing.T) {
	c := assert.NewAborting(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is not on PATH: %v", err)
	}
	modOut, err := exec.Command(goBin, "env", "GOMOD").Output()
	c.NoError(err, "go env GOMOD")
	moduleRoot := filepath.Dir(strings.TrimSpace(string(modOut)))
	c.NotEq("", moduleRoot, "module root")

	cmd := exec.Command(goBin, "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./...")
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	c.NoError(err, "go list ./...")

	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, imp := range fields[1:] {
			if strings.HasPrefix(imp, "github.com/anthropics/anthropic-sdk-go") {
				got = append(got, fields[0])
				break
			}
		}
	}
	sort.Strings(got)
	c.EqDeep(sdkImporters, got, "Anthropic SDK importers")
}
