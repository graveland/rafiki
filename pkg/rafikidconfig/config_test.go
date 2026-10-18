// SPDX-License-Identifier: Apache-2.0

package rafikidconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/rafikidconfig"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

// write puts body in a fresh temp file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rafikid.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadMissingFileIsZero(t *testing.T) {
	got, err := rafikidconfig.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assert.NewCollecting(t).EqDeep(rafikidconfig.File{}, got, "missing file must yield the zero File")
}

func TestLoadTelemetrySection(t *testing.T) {
	p := write(t, `
[telemetry]
endpoint = "http://collector:4318"
protocol = "grpc"
content = "rules"

[[telemetry.content_rule]]
user = "alice"
provider = "openrouter"

[[telemetry.content_rule]]
model = "anthropic/*"
`)
	got, err := rafikidconfig.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := telemetry.Config{
		Endpoint: "http://collector:4318",
		Protocol: "grpc",
		Content:  "rules",
		Rules: []telemetry.Rule{
			{User: "alice", Provider: "openrouter"},
			{Model: "anthropic/*"},
		},
	}
	assert.NewCollecting(t).EqDeep(want, got.Telemetry, "telemetry section")
}

func TestLoadUnknownKeyFails(t *testing.T) {
	p := write(t, "[telemetry]\nendpont = \"x\"\n")
	_, err := rafikidconfig.Load(p)
	if err == nil {
		t.Fatal("Load accepted an unknown key")
	}
	if want := "telemetry.endpont"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
}

func TestLoadUnknownSectionFails(t *testing.T) {
	p := write(t, "[nope]\na = 1\n")
	_, err := rafikidconfig.Load(p)
	if err == nil {
		t.Fatal("Load accepted an unknown section")
	}
	if want := "nope.a"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
}
