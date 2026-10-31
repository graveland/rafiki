// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"go.graveland.dev/rafiki/pkg/msg/responses"
)

// This file keeps the names callers already use for the Responses signature
// format, now that the format itself lives in pkg/msg/responses.

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
