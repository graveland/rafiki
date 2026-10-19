package server

import (
	"errors"
	"fmt"
	"strings"
)

// Route is the upstream the messages proxy sends a request to.
type Route string

const (
	RouteAnthropic  Route = "anthropic"
	RouteOpenRouter Route = "openrouter"
)

var (
	// ErrPassthroughNonAnthropic refuses a passthrough request for a model only
	// OpenRouter serves: the caller's Anthropic credential cannot pay
	// OpenRouter, and falling back to rafiki's key would bill the party the
	// caller explicitly opted out of billing.
	ErrPassthroughNonAnthropic = errors.New("passthrough auth cannot serve non-Anthropic model")
	// ErrOpenRouterNotConfigured refuses an OpenRouter model when the proxy has
	// no OpenRouter key.
	ErrOpenRouterNotConfigured = errors.New("openrouter not configured for model")
)

// RouteModel decides where the proxy sends model, which must already be
// resolved by routing.ResolveModel (that is what turns "anthropic/<id>" and
// "<family>-latest" into a bare Anthropic id). A slash id is an
// OpenRouter-native model; anything else goes to Anthropic. passthrough is
// whether the request carries the caller's own Anthropic credential, openRouter
// whether the proxy has an OpenRouter key.
//
// It is the proxy's whole routing decision, shared with the daemon's spawn-time
// check so the two cannot disagree.
func RouteModel(model string, passthrough, openRouter bool) (Route, error) {
	if !strings.Contains(model, "/") {
		return RouteAnthropic, nil
	}
	if passthrough {
		return "", fmt.Errorf("%w %s", ErrPassthroughNonAnthropic, model)
	}
	if !openRouter {
		return "", fmt.Errorf("%w %s", ErrOpenRouterNotConfigured, model)
	}
	return RouteOpenRouter, nil
}
