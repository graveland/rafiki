// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

// payload decodes a JSON fixture the way the gateway does, keeping numbers as
// their literal text.
func payload(t *testing.T, text string) any {
	t.Helper()
	value, err := parsePayload([]byte(text))
	if err != nil {
		t.Fatalf("parsePayload(%q): %v", text, err)
	}
	return value
}

func TestRulesLookup(t *testing.T) {
	body := payload(t, `{
		"action": "failure",
		"number": 42,
		"ratio": 1.50,
		"flag": true,
		"nested": {"inner": {"name": "deploy"}},
		"list": [{"id": 7}, "second"]
	}`)

	t.Run("nested object", func(t *testing.T) {
		c := assert.NewAborting(t)
		value, ok := lookup(body, "nested.inner.name")
		c.True(ok)
		c.Eq("deploy", value)
	})

	t.Run("array index", func(t *testing.T) {
		c := assert.NewAborting(t)
		value, ok := lookup(body, "list.0.id")
		c.True(ok)
		c.Eq("7", value)
		value, ok = lookup(body, "list.1")
		c.True(ok)
		c.Eq("second", value)
	})

	t.Run("numbers keep their JSON text form", func(t *testing.T) {
		c := assert.NewAborting(t)
		value, ok := lookup(body, "number")
		c.True(ok)
		c.Eq("42", value)
		// The literal the sender wrote, not a float64 round trip.
		value, ok = lookup(body, "ratio")
		c.True(ok)
		c.Eq("1.50", value)
	})

	t.Run("bool", func(t *testing.T) {
		c := assert.NewAborting(t)
		value, ok := lookup(body, "flag")
		c.True(ok)
		c.Eq("true", value)
	})

	t.Run("float64 from a plain decode", func(t *testing.T) {
		c := assert.NewAborting(t)
		var plain any
		if err := json.Unmarshal([]byte(`{"n": 1.5}`), &plain); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		value, ok := lookup(plain, "n")
		c.True(ok)
		c.Eq("1.5", value)
	})

	t.Run("missing paths", func(t *testing.T) {
		c := assert.NewAborting(t)
		for _, path := range []string{
			"nope", "nested.nope", "nested.inner.name.deeper", "list.5", "list.-1",
			"list.notanindex", "number.deeper", "", "list.0.id.deeper",
		} {
			_, ok := lookup(body, path)
			c.False(ok, "path %q should be absent", path)
		}
	})

	t.Run("object and array are not scalars", func(t *testing.T) {
		c := assert.NewAborting(t)
		_, ok := lookup(body, "nested.inner")
		c.False(ok)
		_, ok = lookup(body, "list")
		c.False(ok)
	})
}

func TestRulesMatch(t *testing.T) {
	body := payload(t, `{"action": "failure", "workflow_run": {"head_branch": "main", "conclusion": "failure"}}`)

	t.Run("event and every match key must hold", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{
			Event: "workflow_run",
			Match: map[string]string{"action": "failure", "workflow_run.head_branch": "main"},
		}
		c.True(rule.matches("workflow_run", body))
	})

	t.Run("one failing match key refuses", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{
			Event: "workflow_run",
			Match: map[string]string{"action": "failure", "workflow_run.head_branch": "release"},
		}
		c.False(rule.matches("workflow_run", body))
	})

	t.Run("an absent match path refuses", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{Match: map[string]string{"workflow_run.head_sha": ""}}
		c.False(rule.matches("workflow_run", body))
	})

	t.Run("event mismatch refuses even with match keys holding", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{
			Event: "workflow_run",
			Match: map[string]string{"action": "failure"},
		}
		c.False(rule.matches("push", body))
	})

	t.Run("empty event matches any event", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.True((Rule{}).matches("push", body))
		c.True((Rule{}).matches("", body))
	})
}

func TestRulesBuild(t *testing.T) {
	body := payload(t, `{"repository": {"full_name": "graveland/rafiki"}, "workflow_run": {"id": 1234, "name": "ci"}, "ratio": 1.50}`)

	t.Run("key interpolation", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{Key: "github/workflow_run/{repository.full_name}/{workflow_run.id}"}
		key, fields := rule.build(body)
		c.Eq("github/workflow_run/graveland/rafiki/1234", key)
		c.Empty(fields)
	})

	t.Run("text outside braces is verbatim, absent paths render empty", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{Key: "prefix:{workflow_run.name}:{nope}:{ratio}:{unclosed"}
		key, _ := rule.build(body)
		c.Eq("prefix:ci::1.50:{unclosed", key)
	})

	t.Run("fields read the configured paths", func(t *testing.T) {
		c := assert.NewAborting(t)
		rule := Rule{Fields: map[string]string{
			"repo":     "repository.full_name",
			"workflow": "workflow_run.name",
			"missing":  "no.such.path",
		}}
		key, fields := rule.build(body)
		c.Eq("", key)
		c.Eq("graveland/rafiki", fields["repo"])
		c.Eq("ci", fields["workflow"])
		// A missing path renders empty rather than dropping the field, so a
		// notification's shape does not change with the sender's payload.
		c.HasKey(fields, "missing")
		c.Eq("", fields["missing"])
	})
}

func TestRulesFirstMatchWins(t *testing.T) {
	c := assert.NewAborting(t)
	body := payload(t, `{"action": "failure"}`)
	rules := []Rule{
		{Event: "workflow_run", Match: map[string]string{"action": "success"}, Key: "success"},
		{Event: "workflow_run", Key: "any-workflow"},
		{Key: "catch-all"},
	}
	rule, ok := firstMatch(rules, "workflow_run", body)
	c.True(ok)
	c.Eq("any-workflow", rule.Key)

	rule, ok = firstMatch(rules, "push", body)
	c.True(ok)
	c.Eq("catch-all", rule.Key)

	_, ok = firstMatch([]Rule{{Event: "workflow_run"}}, "push", body)
	c.False(ok)
}
