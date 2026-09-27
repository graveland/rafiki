// SPDX-License-Identifier: Apache-2.0

package clientstate_test

import (
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"

	"github.com/multigres/testkit/assert"
)

// Update is read-modify-write, which is the whole reason it exists: a writer
// that marshalled only its own section would drop every other one.
func TestUpdatePreservesSectionsItDoesNotTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	sc := clientstate.Scope{Profile: "test"}
	clientstate.UpdateScoped(sc, func(s *clientstate.State) {
		s.ModelView = &clientstate.ModelView{ToolsOnly: true}
	})
	clientstate.RememberModel("test", "fundi", "z-ai/glm-5.3-flash")

	got := clientstate.LoadScoped(sc)
	c.False(got.ModelView == nil || !got.ModelView.ToolsOnly, "remembering a model dropped the modelView section")
	c.Eq("z-ai/glm-5.3-flash", got.LastModel["fundi"], "LastModel = %v", got.LastModel)
}

// Keyed by KIND: a claude child cannot resolve an OpenRouter id, so one
// remembered model across both kinds would eventually prefill a spawn that
// attaches and never answers.
func TestLastModelIsPerKind(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	clientstate.RememberModel("test", "fundi", "z-ai/glm-5.3-flash")
	clientstate.RememberModel("test", "claude", "anthropic/claude-opus-5")

	c.Eq("z-ai/glm-5.3-flash", clientstate.LastModelFor("test", "fundi"), "fundi =")
	c.Eq("anthropic/claude-opus-5", clientstate.LastModelFor("test", "claude"), "claude =")
	c.Eq("", clientstate.LastModelFor("test", "unseen"), "unseen kind")
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
	c := assert.NewAborting(t)
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	clientstate.RememberModel("work", "claude", "claude-opus-5")
	clientstate.RememberModel("personal", "fundi", "openrouter/cheap-model")

	c.Eq("claude-opus-5", clientstate.LastModelFor("work", "claude"), "work/claude =")
	c.Eq("openrouter/cheap-model", clientstate.LastModelFor("personal", "fundi"), "personal/fundi =")
	// The whole point: one profile's memory must not answer for the other.
	c.Eq("", clientstate.LastModelFor("work", "fundi"), "work/fundi")
}

func TestModelViewIsPerProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	clientstate.UpdateScoped(clientstate.Scope{Profile: "work"}, func(s *clientstate.State) {
		s.ModelView = &clientstate.ModelView{ToolsOnly: true, Keys: []clientstate.SortKey{{Field: "cost"}}}
	})
	assert.NewAborting(t).Nil(clientstate.LoadScoped(clientstate.Scope{Profile: "personal"}).ModelView, "personal inherited work's model view")
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
	c := assert.NewAborting(t)
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
	c.False(got.ModelView == nil || !got.ModelView.VisionOnly, "the second Update dropped the first's section")
	c.Eq("m", got.LastModel["fundi"], "the second Update did not persist")
}

func TestRememberAndRecallExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	c.Eq("", clientstate.LastExecutorFor("work", "claude"), "want empty before anything is remembered, got")
	clientstate.RememberExecutor("work", "claude", "greyshift")
	c.Eq("greyshift", clientstate.LastExecutorFor("work", "claude"), "want greyshift, got")
	// Different kind, different profile: neither leaks into the other.
	c.Eq("", clientstate.LastExecutorFor("work", "fundi"), "kind must not leak across kinds, got")
	c.Eq("", clientstate.LastExecutorFor("home", "claude"), "profile must not leak across profiles, got")
}

func TestRememberExecutorNoOpsOnEmptyArgs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	clientstate.RememberExecutor("", "claude", "greyshift")
	clientstate.RememberExecutor("work", "", "greyshift")
	clientstate.RememberExecutor("work", "claude", "")
	assert.NewAborting(t).Eq("", clientstate.LastExecutorFor("work", "claude"), "no-op cases must not have written anything, got")
}
