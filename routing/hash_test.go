package routing

import "testing"

func TestPrefixHash(t *testing.T) {
	base := []byte(`{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"a"},{"name":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	h := PrefixHash(base)
	if h == "" {
		t.Fatal("expected a hash")
	}

	// Message-invariant: a different/growing message list must not change the prefix hash.
	moreMsgs := []byte(`{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"a"},{"name":"b"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`)
	if PrefixHash(moreMsgs) != h {
		t.Error("prefix hash changed when only the message list changed")
	}

	// Top-level key order is irrelevant (map marshal canonicalizes).
	reordered := []byte(`{"tools":[{"name":"a"},{"name":"b"}],"messages":[],"system":"you are helpful","model":"claude-sonnet-5"}`)
	if PrefixHash(reordered) != h {
		t.Error("prefix hash changed on top-level key reorder (should be canonical)")
	}

	// Tool ARRAY order IS significant — a reordered tools array is a different prefix
	// (this is the property that catches non-deterministic tool ordering).
	toolsSwapped := []byte(`{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"b"},{"name":"a"}],"messages":[]}`)
	if PrefixHash(toolsSwapped) == h {
		t.Error("prefix hash did NOT change when the tools array was reordered")
	}

	// A changed system prompt is a different prefix.
	sysChanged := []byte(`{"model":"claude-sonnet-5","system":"you are DIFFERENT","tools":[{"name":"a"},{"name":"b"}],"messages":[]}`)
	if PrefixHash(sysChanged) == h {
		t.Error("prefix hash did NOT change when the system prompt changed")
	}

	// Unparseable request → empty (best-effort; column is nullable).
	if PrefixHash([]byte("not json")) != "" {
		t.Error("expected empty hash for unparseable request")
	}
}
