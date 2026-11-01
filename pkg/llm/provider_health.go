// SPDX-License-Identifier: Apache-2.0

package llm

// This file is where an attempt's evidence for the provider guard is built:
// the decision a request's provider object was built from, and the observation
// each attempt reports. Keeping both here means the body a request carried, the
// turn row it wrote and the evidence the guard received can never disagree
// about which provider the attempt was pinned to.

import (
	"encoding/json"
	"errors"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/routing"
)

// providerDecision is prepareSend's verdict about the provider object it built
// for one request — computed ONCE, alongside the routing spec, and threaded to
// every later stage (the rate gate's key, the turn row, the guard's evidence)
// so none of them re-derives it.
type providerDecision struct {
	// pin is the provider slug the request was pinned to with only=[slug] by the
	// guard (routing.ProviderGuard.PinFor), or "" when OpenRouter chose.
	pin string
	// hadAutomatic reports that no pin was sent but the guard's own ejections
	// were: the line still had somewhere to go once they were lifted.
	hadAutomatic bool
	// model is the resolved provider-local model id every guard call for this
	// request used (PinFor, ExclusionsFor, IgnoredFor and the observations).
	model string
}

// failureDetailBytes bounds the error excerpt carried into ban evidence. The
// guard clips to the same figure on the way into the durable log; clipping here
// keeps the observation itself small.
const failureDetailBytes = 300

// successObservation is the guard's evidence for one attempt that returned a
// response. It is built from the request AND the response: the output cap rule
// needs both (the max_tokens asked for and the tokens produced), and the
// repetition and tool-corruption rules need what the model actually wrote.
func successObservation(ref turnRef, d providerDecision, req *msg.Request, resp *msg.Response) routing.Observation {
	return routing.Observation{
		Provider:        ProviderOf(resp),
		Model:           d.model,
		Conversation:    ref.convID,
		PrefixHash:      ref.prefixHash,
		InputTokens:     msg.Val(resp.Usage.Input),
		CacheReadTokens: msg.Val(resp.Usage.CacheRead),
		Pinned:          d.pin,
		StopReason:      resp.StopReason.String(),
		OutputTokens:    msg.Val(resp.Usage.Output),
		MaxTokens:       req.MaxTokens,
		Repetition:      repetition(resp),
		ToolArgsInvalid: invalidToolArgs(req.Tools, resp.Content),
	}
}

// failureObservation is the guard's evidence for one attempt that failed. There
// is no response, so the provider is deliberately left unnamed: a failure is
// attributed ONLY through Pinned (see routing.ProviderGuard.Observe), and an
// unpinned failure says nothing about which provider would have been at fault.
func failureObservation(ref turnRef, d providerDecision, err error) routing.Observation {
	return routing.Observation{
		Model:         d.model,
		Conversation:  ref.convID,
		Pinned:        d.pin,
		Failed:        true,
		FailureKind:   failureKind(err, d.pin != ""),
		FailureDetail: clipFailureDetail(err),
	}
}

// fallbackError marks an error produced by the FALLBACK chain rather than by
// the attempt this request's providerDecision describes. It wraps, so every
// classifier (errors.As for *msg.APIError, FailoverWorthy, the retry rules)
// still sees the real error; it exists so the no-eligible resend can tell a
// refusal of the pinned request from a refusal of a fallback nobody pinned.
type fallbackError struct{ err error }

func (e fallbackError) Error() string { return e.err.Error() }
func (e fallbackError) Unwrap() error { return e.err }

// clipFailureDetail is the first failureDetailBytes of err's message, or "" for
// a nil error.
func clipFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) <= failureDetailBytes {
		return s
	}
	return s[:failureDetailBytes]
}

// failureKind maps an upstream error to the guard's vocabulary.
//
//	nil                                                      → FailureNone
//	*msg.APIError Kind ErrTransient, ErrOverloaded           → FailureTransient
//	*msg.APIError Kind ErrRateLimit, pinned                  → FailureTransient (that provider's capacity)
//	*msg.APIError Kind ErrRateLimit, unpinned                → FailureNotProvider (cannot attribute)
//	*msg.APIError Kind ErrNoEligible                         → FailureNoEligible
//	*msg.APIError Kind ErrUsageLimit, ErrPromptTooLarge, ErrOther → FailureNotProvider
//	context.DeadlineExceeded, or any non-APIError error (transport) → FailureTransient
//
// The zero kind, FailureNone, is the fail-safe: an observation that forgot to
// classify an error can never ban a provider.
func failureKind(err error, pinned bool) routing.FailureKind {
	if err == nil {
		return routing.FailureNone
	}
	var apiErr *msg.APIError
	if !errors.As(err, &apiErr) {
		// A transport failure (a timeout, a reset connection) is the network's,
		// not the provider's account or request shape.
		return routing.FailureTransient
	}
	switch apiErr.Kind {
	case msg.ErrTransient, msg.ErrOverloaded:
		return routing.FailureTransient
	case msg.ErrRateLimit:
		if pinned {
			// A pinned provider's 429 is that provider's own capacity.
			return routing.FailureTransient
		}
		return routing.FailureNotProvider
	case msg.ErrNoEligible:
		return routing.FailureNoEligible
	default:
		// ErrUsageLimit, ErrPromptTooLarge, ErrOther: the account's limit, the
		// request's shape or our own routing spec. Never the provider.
		return routing.FailureNotProvider
	}
}

// repetition is RepetitionScore over the concatenation of every Text and
// Thinking block's text in resp, separated by "\n"; nil when not scored (the
// text is too short to judge, or carries no words). A nil pointer, never 0:
// 0 is maximal repetition.
func repetition(resp *msg.Response) *float64 {
	if resp == nil {
		return nil
	}
	parts := make([]string, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch v := b.(type) {
		case msg.Text:
			parts = append(parts, v.Text)
		case msg.Thinking:
			parts = append(parts, v.Text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	score, ok := routing.RepetitionScore(strings.Join(parts, "\n"))
	if !ok {
		return nil
	}
	return &score
}

// invalidToolArgs counts ToolUse blocks in content whose Input is not a JSON
// object, or lacks a key listed in the "required" array of the InputSchema of
// the ToolDef with the same Name. A ToolUse naming no ToolDef, or a schema
// without "required", counts as valid.
func invalidToolArgs(tools []msg.ToolDef, content []msg.Block) int {
	required := make(map[string][]string, len(tools))
	for _, t := range tools {
		if len(t.InputSchema) == 0 {
			continue
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil || len(schema.Required) == 0 {
			continue
		}
		required[t.Name] = schema.Required
	}
	invalid := 0
	for _, b := range content {
		tu, ok := b.(msg.ToolUse)
		if !ok {
			continue
		}
		want, checked := required[tu.Name]
		if !checked {
			continue
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(tu.Input, &input); err != nil {
			invalid++
			continue
		}
		for _, key := range want {
			if _, present := input[key]; !present {
				invalid++
				break
			}
		}
	}
	return invalid
}
