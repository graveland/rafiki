// SPDX-License-Identifier: Apache-2.0

package clientstate_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

// Update is read-modify-write, which is the whole reason it exists: a writer
// that marshalled only its own section would drop every other one.
func TestUpdatePreservesSectionsItDoesNotTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	clientstate.Update(func(s *clientstate.State) {
		s.ModelView = &clientstate.ModelView{ToolsOnly: true}
	})
	clientstate.RememberModel("fundi", "z-ai/glm-5.3-flash")

	got := clientstate.Load()
	if got.ModelView == nil || !got.ModelView.ToolsOnly {
		t.Error("remembering a model dropped the modelView section")
	}
	if got.LastModel["fundi"] != "z-ai/glm-5.3-flash" {
		t.Errorf("LastModel = %v", got.LastModel)
	}
}

// Keyed by KIND: a claude child cannot resolve an OpenRouter id, so one
// remembered model across both kinds would eventually prefill a spawn that
// attaches and never answers.
func TestLastModelIsPerKind(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	clientstate.RememberModel("fundi", "z-ai/glm-5.3-flash")
	clientstate.RememberModel("claude", "anthropic/claude-opus-5")

	if got := clientstate.LastModelFor("fundi"); got != "z-ai/glm-5.3-flash" {
		t.Errorf("fundi = %q", got)
	}
	if got := clientstate.LastModelFor("claude"); got != "anthropic/claude-opus-5" {
		t.Errorf("claude = %q", got)
	}
	if got := clientstate.LastModelFor("unseen"); got != "" {
		t.Errorf("unseen kind = %q, want empty", got)
	}
}

// "The daemon's default" is not a choice worth replaying, and storing it would
// pin whatever that default happened to be on the day.
func TestEmptyModelIsNotRemembered(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	clientstate.RememberModel("fundi", "")
	clientstate.RememberModel("", "some/model")

	if s := clientstate.Load(); len(s.LastModel) != 0 {
		t.Errorf("LastModel = %v, want nothing recorded", s.LastModel)
	}
}

// A preferences file must never be able to stop the client starting.
func TestLoadIsTotalOnAMissingFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if s := clientstate.Load(); s.ModelView != nil || len(s.LastModel) != 0 {
		t.Errorf("Load on an empty dir = %+v, want a zero State", s)
	}
}

// Currency goes through Update like every other section, so it must survive
// alongside one already set -- the same read-modify-write guarantee
// TestUpdatePreservesSectionsItDoesNotTouch pins for ModelView/LastModel.
func TestCurrencySurvivesAlongsideOtherSections(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	clientstate.RememberModel("fundi", "z-ai/glm-5.3-flash")
	clientstate.Update(func(s *clientstate.State) {
		s.Currency = &clientstate.Currency{Code: "CAD", Rate: 1.38}
	})

	got := clientstate.Load()
	if got.Currency == nil || got.Currency.Code != "CAD" || got.Currency.Rate != 1.38 {
		t.Errorf("Currency = %+v", got.Currency)
	}
	if got.LastModel["fundi"] != "z-ai/glm-5.3-flash" {
		t.Errorf("setting Currency dropped LastModel: %v", got.LastModel)
	}
}
