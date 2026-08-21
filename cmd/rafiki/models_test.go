package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// The default kind is "fundi", so an unset --kind must offer exactly what the
// native runtime can route — and must NOT offer pi's provider-local ids, which
// would route to OpenRouter and fail there.
func TestModelSourcesForKind_DefaultIsAgent(t *testing.T) {
	for _, kind := range []string{"", protocol.KindFundi} {
		got := modelSourcesForKind(kind)
		if !got[models.SourceOpenRouter] || !got[models.SourceBuiltin] {
			t.Errorf("kind %q must offer the curated list and OpenRouter slash ids, got %v", kind, got)
		}
		if got[models.SourceUserConfig] {
			t.Errorf("kind %q offers pi's provider-local ids, which the native runtime cannot route", kind)
		}
	}
}

// A pi child resolves models against its own providers, so it must not be
// offered OpenRouter ids or the curated list, whose provider names a given pi
// install has no reason to have configured. Offering them produced a child that
// spawned, attached, and then never answered.
func TestModelSourcesForKind_Pi(t *testing.T) {
	got := modelSourcesForKind(protocol.KindPi)
	if got[models.SourceOpenRouter] {
		t.Error("pi offered OpenRouter ids it cannot resolve")
	}
	if got[models.SourceBuiltin] {
		t.Error("pi offered the curated list, whose providers it need not have")
	}
	if !got[models.SourceUserConfig] {
		t.Error("pi omits ~/.pi/agent/models.json, the only source it is guaranteed to resolve")
	}
}

func TestModelSourcesForKind_Agent(t *testing.T) {
	got := modelSourcesForKind(protocol.KindFundi)
	if !got[models.SourceOpenRouter] || !got[models.SourceBuiltin] {
		t.Errorf("fundi must offer the curated list and OpenRouter slash ids, got %v", got)
	}
	// pi's provider-local ids are meaningless to the native runtime: an
	// "anthropic-work/" prefix is not "anthropic/", so it would route to
	// OpenRouter and fail there.
	if got[models.SourceUserConfig] {
		t.Error("fundi must not offer pi's provider-local ids")
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
	for _, kind := range []string{"", protocol.KindPi, protocol.KindFundi, protocol.KindClaude, "nonsense"} {
		if len(modelSourcesForKind(kind)) == 0 {
			t.Errorf("kind %q yields no sources", kind)
		}
	}
}
