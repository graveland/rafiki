// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/msg/responses"
)

// This file keeps the names callers already use for the Responses signature
// format, now that the format itself lives in pkg/msg/responses. The one
// SDK-shaped helper left here, StripForeignThinking, exists only because
// pkg/batch still builds SDK params; it goes away with that path.

// ResponsesSignaturePrefix starts every thinking signature written for an
// OpenAI Responses reasoning item. The whole signature is
// "<prefix><mode>:<item id>:<encrypted content>": the dialect that minted the
// blob, the item id the endpoint needs to accept a resent reasoning item, and
// the opaque blob it encrypts the model's reasoning into.
const ResponsesSignaturePrefix = "openai-responses:"

// ResponsesSignatureMode names the Responses backend dialect that minted a
// thinking signature's encrypted content. It is a closed set: a signature
// naming any other mode does not parse, because there is no backend here that
// could resume such a blob.
type ResponsesSignatureMode = responses.Mode

const (
	// ResponsesModeAPI is the plain openai-responses kind: api.openai.com under
	// an operator's key, or a compatible endpoint.
	ResponsesModeAPI = responses.ModeAPI
	// ResponsesModeChatGPT is the openai-responses-chatgpt subscription dialect,
	// whose blobs only the ChatGPT backend accepts.
	ResponsesModeChatGPT = responses.ModeChatGPT
)

// responsesSignature renders the thinking signature for one reasoning item
// minted by mode.
func responsesSignature(mode ResponsesSignatureMode, itemID, encryptedContent string) string {
	return responses.Signature(mode, itemID, encryptedContent)
}

// IsResponsesSignature reports whether a thinking signature was produced by a
// Responses sender, whichever mode minted it. Deliberately mode-blind: the
// Anthropic direction must drop every Responses signature, because Anthropic
// can verify none of them.
func IsResponsesSignature(sig string) bool { return responses.IsSignature(sig) }

// ParseResponsesSignature splits "openai-responses:<mode>:<id>:<blob>";
// ok=false on any other shape, including the older two-field
// "<prefix><id>:<blob>" form and an unknown mode.
func ParseResponsesSignature(sig string) (mode ResponsesSignatureMode, id, blob string, ok bool) {
	return responses.ParseSignature(sig)
}

// StripForeignThinking returns a copy of msgs keeping only thinking blocks whose
// signature belongs to the target family: forResponses=true keeps every
// Responses signature (whichever mode), false keeps only the others. Other
// blocks are untouched, and the caller's slice is never mutated.
//
// A conversation started on OpenAI Responses and continued on Anthropic carries
// reasoning items whose signatures Anthropic cannot verify; a conversation
// started on Anthropic and retried on OpenAI carries signatures that are not
// item ids at all. Neither side rejects the request cleanly: Anthropic errors on
// a signature it did not mint, and OpenAI treats an unknown reasoning id as
// nothing to resume, which silently loses the reasoning. Dropping the foreign
// block is the one behavior both sides agree on.
//
// The native request path does this in the Anthropic codec; this SDK-typed copy
// exists for pkg/batch's parked requests and disappears with them.
func StripForeignThinking(msgs []anthropic.MessageParam, forResponses bool) []anthropic.MessageParam {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]anthropic.MessageParam, len(msgs))
	for i := range msgs {
		// Copy the struct (role, cache_control, ...) and replace content: the
		// caller's Content slice is never written to.
		out[i] = msgs[i]
		kept := make([]anthropic.ContentBlockParamUnion, 0, len(msgs[i].Content))
		for _, b := range msgs[i].Content {
			if b.OfThinking != nil && IsResponsesSignature(b.OfThinking.Signature) != forResponses {
				continue
			}
			kept = append(kept, b)
		}
		out[i].Content = kept
	}
	return out
}
