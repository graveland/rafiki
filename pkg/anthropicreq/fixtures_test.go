// SPDX-License-Identifier: Apache-2.0

package anthropicreq

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// inlineFixtures are the request bodies the accessor and prefix-hash tests pin
// by name. Each one exists because some shape of the rules is only visible in
// it; a fixture added here is asserted against EVERY accessor's oracle.
var inlineFixtures = map[string]string{
	"no system":                `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`,
	"string system":            `{"model":"m","system":"you are helpful","messages":[]}`,
	"array system":             `{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.267.019; cc_entrypoint=cli; cc_is_subagent=true; cc_prev_req=msg_1"}],"messages":[]}`,
	"array system no billing":  `{"model":"m","system":[{"type":"text","text":"plain instructions"}],"messages":[]}`,
	"empty array system":       `{"model":"m","system":[],"messages":[]}`,
	"tools with null schema":   `{"model":"m","tools":[{"name":"web_search","input_schema":null}],"messages":[]}`,
	"tools with real schema":   `{"model":"m","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[]}`,
	"tools absent":             `{"model":"m","messages":[]}`,
	"unicode and escapes":      `{"model":"m","system":"héllo — \"quoted\" \\ backslash \u00e9 ✓","messages":[{"role":"user","content":"a < b & \"c\""}]}`,
	"integer above 2^53":       `{"model":"m","max_tokens":9007199254740993,"seed":12345678901234567890,"messages":[]}`,
	"floats":                   `{"model":"m","temperature":0.7,"top_p":0.90000000000000002,"messages":[]}`,
	"nested keys out of order": `{"model":"m","metadata":{"z":1,"a":{"y":[1,2,{"b":true,"a":false}]}},"messages":[]}`,
	"empty object":             `{}`,
	"only messages":            `{"messages":[{"role":"user","content":"hi"}]}`,
	"json null body":           `null`,
	"diagnostics":              `{"model":"m","diagnostics":{"previous_message_id":"msg_abc"},"messages":[]}`,
	"messages null":            `{"model":"m","messages":null}`,
	"messages with breakpoints": `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":"plain string mentioning \\\"cache_control\\\""},` +
		`{"role":"user","content":[{"type":"text","text":"b"}]}]}`,
	"role is not a string": `{"model":"m","messages":[{"role":1,"content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`,
	"stream true":          `{"model":"m","stream":true,"messages":[]}`,
	"unparseable body":     `not json`,
}

// bodyFixtures returns every fixture these tests pin, named: the inline table
// plus every request body under pkg/capture/testdata and pkg/server/testdata.
// Nothing is skipped — a fixture directory that holds a body must be covered —
// so a new recorded body is picked up without touching the tests.
func bodyFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(inlineFixtures))
	for name, body := range inlineFixtures {
		out[name] = []byte(body)
	}
	for _, dir := range []string{"../capture/testdata", "../server/testdata"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		sort.Strings(paths)
		for _, p := range paths {
			body, rerr := os.ReadFile(p)
			if rerr != nil {
				t.Fatalf("read fixture %s: %v", p, rerr)
			}
			out[p] = body
		}
		t.Logf("fixtures from %s: %d", dir, len(paths))
	}
	return out
}
