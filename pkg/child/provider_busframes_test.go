package child

import (
	"bytes"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestIdentityProvider_BusFrames_Identity asserts the pi provider is a pass-through on
// the bus: pi's stdout already IS the AgentSessionEvent stream, so BusFrames
// returns the raw line unchanged.
func TestIdentityProvider_BusFrames_Identity(t *testing.T) {
	line := []byte(`{"type":"message_end","message":{"role":"assistant"}}`)
	frames := IdentityProvider{}.BusFrames(line, 100)
	assert.NewAborting(t).Len(frames, 1, "pi BusFrames should return exactly 1 frame, got %d", len(frames))
	if !bytes.Equal(frames[0], line) {
		t.Fatalf("pi BusFrames must be identity, got %q want %q", frames[0], line)
	}
}

// TestProviderFresh_PerChildInstance asserts every provider exposes a Fresh()
// factory so each Child gets its own provider instance (per-child translator
// state must not leak across spawns). pi is stateless so Fresh may return an
// equivalent value; the contract is only that Fresh returns a usable provider.
func TestProviderFresh_PerChildInstance(t *testing.T) {
	c := assert.NewAborting(t)
	c.NotNil((IdentityProvider{}).Fresh(), "identityProvider.Fresh returned nil")
	c.NotNil((ClaudeProvider{}).Fresh(), "ClaudeProvider.Fresh returned nil")
	// Two fresh claude providers must be distinct instances so per-child state is
	// isolated.
	a := (ClaudeProvider{}).Fresh()
	b := (ClaudeProvider{}).Fresh()
	c.False(a == b, "ClaudeProvider.Fresh must return distinct per-child instances")
}

func TestProviderNormalizes(t *testing.T) {
	c := assert.NewAborting(t)
	c.False((IdentityProvider{}).Normalizes(), "identityProvider.Normalizes() = true, want false (stdout is already pi-vocabulary)")
	c.False(!(ClaudeProvider{}.Normalizes()), "ClaudeProvider.Normalizes() = false, want true (translates claude→pi)")
	c.True((ClaudeProvider{}).Fresh().Normalizes(), "claudeProvider (Fresh) Normalizes() = false, want true")
}
