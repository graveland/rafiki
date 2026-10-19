package server

import (
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestRouteModel(t *testing.T) {
	cases := []struct {
		name                    string
		model                   string
		passthrough, openRouter bool
		want                    Route
		wantErr                 error
	}{
		{"bare id goes to anthropic", "claude-opus-5-5", false, false, RouteAnthropic, nil},
		{"bare id under passthrough", "claude-opus-5-5", true, false, RouteAnthropic, nil},
		{"slash id goes to openrouter", "openai/gpt-5.6-sol", false, true, RouteOpenRouter, nil},
		{"slash id under passthrough", "openai/gpt-5.6-sol", true, true, "", ErrPassthroughNonAnthropic},
		{"slash id with no openrouter key", "openai/gpt-5.6-sol", false, false, "", ErrOpenRouterNotConfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			got, err := RouteModel(tc.model, tc.passthrough, tc.openRouter)
			ck.Eq(tc.want, got, "route")
			ck.True(errors.Is(err, tc.wantErr), "error: got %v, want %v", err, tc.wantErr)
		})
	}
}
