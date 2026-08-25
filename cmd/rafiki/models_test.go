package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// The default kind is "fundi", so an unset --kind must offer exactly what the
// native runtime can route — and must NOT offer provider-local ids, which would
// route to OpenRouter and fail there.
func TestModelSourcesForKind_DefaultIsAgent(t *testing.T) {
	for _, kind := range []string{"", protocol.KindFundi} {
		got := modelSourcesForKind(kind)
		if !got[models.SourceOpenRouter] || !got[models.SourceBuiltin] {
			t.Errorf("kind %q must offer the curated list and OpenRouter slash ids, got %v", kind, got)
		}
		if got[models.SourceUserConfig] {
			t.Errorf("kind %q offers provider-local ids, which the native runtime cannot route", kind)
		}
	}
}

func TestModelSourcesForKind_Agent(t *testing.T) {
	got := modelSourcesForKind(protocol.KindFundi)
	if !got[models.SourceOpenRouter] || !got[models.SourceBuiltin] {
		t.Errorf("fundi must offer the curated list and OpenRouter slash ids, got %v", got)
	}
	// Provider-local ids are meaningless to the native runtime: an
	// "anthropic-work/" prefix is not "anthropic/", so it would route to
	// OpenRouter and fail there.
	if got[models.SourceUserConfig] {
		t.Error("fundi must not offer provider-local ids")
	}
}

func TestModelSourcesForKind_Claude(t *testing.T) {
	got := modelSourcesForKind(protocol.KindClaude)
	if !got[models.SourceBuiltin] {
		t.Error("claude must offer the curated Anthropic ids")
	}
	for _, s := range []models.Source{models.SourceOpenRouter, models.SourceUserConfig, models.SourceLocal} {
		if got[s] {
			t.Errorf("claude must not offer %s: Claude Code only knows Anthropic ids", s)
		}
	}
}

// Every kind must resolve to a non-empty source set, or completion silently
// offers nothing at all.
func TestModelSourcesForKind_NeverEmpty(t *testing.T) {
	for _, kind := range []string{"", protocol.KindFundi, protocol.KindClaude, "nonsense"} {
		if len(modelSourcesForKind(kind)) == 0 {
			t.Errorf("kind %q yields no sources", kind)
		}
	}
}
