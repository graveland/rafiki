// SPDX-License-Identifier: Apache-2.0

package clientstate_test

import (
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

// Update is read-modify-write, which is the whole reason it exists: a writer
// that marshalled only its own section would drop every other one.
func TestUpdatePreservesSectionsItDoesNotTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	sc := clientstate.Scope{Profile: "test"}
	clientstate.UpdateScoped(sc, func(s *clientstate.State) {
		s.ModelView = &clientstate.ModelView{ToolsOnly: true}
	})
	clientstate.RememberModel("test", "fundi", "z-ai/glm-5.3-flash")

	got := clientstate.LoadScoped(sc)
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

	clientstate.RememberModel("test", "fundi", "z-ai/glm-5.3-flash")
	clientstate.RememberModel("test", "claude", "anthropic/claude-opus-5")

	if got := clientstate.LastModelFor("test", "fundi"); got != "z-ai/glm-5.3-flash" {
		t.Errorf("fundi = %q", got)
	}
	if got := clientstate.LastModelFor("test", "claude"); got != "anthropic/claude-opus-5" {
		t.Errorf("claude = %q", got)
	}
	if got := clientstate.LastModelFor("test", "unseen"); got != "" {
		t.Errorf("unseen kind = %q, want empty", got)
	}
}

// "The daemon's default" is not a choice worth replaying, and storing it would
// pin whatever that default happened to be on the day.
func TestEmptyModelIsNotRemembered(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	clientstate.RememberModel("test", "fundi", "")
	clientstate.RememberModel("test", "", "some/model")

	if s := clientstate.LoadScoped(clientstate.Scope{Profile: "test"}); len(s.LastModel) != 0 {
		t.Errorf("LastModel = %v, want nothing recorded", s.LastModel)
	}
}

// A preferences file must never be able to stop the client starting.
func TestLoadIsTotalOnAMissingFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if s := clientstate.LoadScoped(clientstate.Scope{Profile: "test"}); s.ModelView != nil || len(s.LastModel) != 0 {
		t.Errorf("LoadScoped on an empty dir = %+v, want a zero State", s)
	}
}

func TestTwoProfilesDoNotShareRememberedState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	clientstate.RememberModel("work", "claude", "claude-opus-5")
	clientstate.RememberModel("personal", "fundi", "openrouter/cheap-model")

	if got := clientstate.LastModelFor("work", "claude"); got != "claude-opus-5" {
		t.Fatalf("work/claude = %q", got)
	}
	if got := clientstate.LastModelFor("personal", "fundi"); got != "openrouter/cheap-model" {
		t.Fatalf("personal/fundi = %q", got)
	}
	// The whole point: one profile's memory must not answer for the other.
	if got := clientstate.LastModelFor("work", "fundi"); got != "" {
		t.Fatalf("work/fundi = %q, want empty — personal's model leaked across profiles", got)
	}
}

func TestModelViewIsPerProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	clientstate.UpdateScoped(clientstate.Scope{Profile: "work"}, func(s *clientstate.State) {
		s.ModelView = &clientstate.ModelView{ToolsOnly: true, Keys: []clientstate.SortKey{{Field: "cost"}}}
	})
	if v := clientstate.LoadScoped(clientstate.Scope{Profile: "personal"}).ModelView; v != nil {
		t.Fatalf("personal inherited work's model view: %+v", v)
	}
	if v := clientstate.LoadScoped(clientstate.Scope{Profile: "work"}).ModelView; v == nil || !v.ToolsOnly {
		t.Fatalf("work lost its own model view: %+v", v)
	}
}

func TestCurrencyIsGlobal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	clientstate.UpdateScoped(clientstate.Scope{}, func(s *clientstate.State) {
		s.Currency = &clientstate.Currency{Code: "CAD", Rate: 1.38}
	})
	// Set from anywhere, read from anywhere: a person's currency does not
	// depend on which daemon answered.
	if c := clientstate.LoadScoped(clientstate.Scope{}).Currency; c == nil || c.Code != "CAD" {
		t.Fatalf("global currency = %+v", c)
	}
}

func TestUpdatePreservesSectionsItDoesNotKnowAbout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	sc := clientstate.Scope{Profile: "work"}
	clientstate.UpdateScoped(sc, func(s *clientstate.State) { s.ModelView = &clientstate.ModelView{VisionOnly: true} })
	clientstate.UpdateScoped(sc, func(s *clientstate.State) {
		if s.LastModel == nil {
			s.LastModel = map[string]string{}
		}
		s.LastModel["fundi"] = "m"
	})
	got := clientstate.LoadScoped(sc)
	if got.ModelView == nil || !got.ModelView.VisionOnly {
		t.Fatal("the second Update dropped the first's section")
	}
	if got.LastModel["fundi"] != "m" {
		t.Fatal("the second Update did not persist")
	}
}
