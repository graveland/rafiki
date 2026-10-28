// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ResponsesSignaturePrefix starts every thinking signature this package writes
// for an OpenAI Responses reasoning item. The whole signature is
// "<prefix><item id>:<encrypted content>": the item id the endpoint needs to
// accept a resent reasoning item, and the opaque blob it encrypts the model's
// reasoning into.
//
// Why a signature at all: an Anthropic thinking block carries exactly two
// strings, thinking and signature, and rafiki stores a turn as Anthropic
// shapes whatever provider produced it. The signature is the only field with
// no meaning of its own, so it is where a foreign provider's continuation
// state rides. On the way back out, the prefix is what tells a provider whose
// state it is: a reasoning item resent to OpenAI as a thinking block, or a
// Claude signature sent to OpenAI as a reasoning id, is at best rejected and at
// worst silently ignored.
const ResponsesSignaturePrefix = "openai-responses:"

// responsesSignatureSeparator joins the item id to the encrypted blob inside a
// signature. The blob is base64url and never contains it; a blob that somehow
// did would keep its colons (ParseResponsesSignature cuts once).
const responsesSignatureSeparator = ":"

// responsesSignature renders the thinking signature for one reasoning item.
func responsesSignature(itemID, encryptedContent string) string {
	return ResponsesSignaturePrefix + itemID + responsesSignatureSeparator + encryptedContent
}

// IsResponsesSignature reports whether a thinking signature was produced by the
// Responses sender.
func IsResponsesSignature(sig string) bool {
	return strings.HasPrefix(sig, ResponsesSignaturePrefix)
}

// ParseResponsesSignature splits "openai-responses:<id>:<blob>"; ok=false on any
// other shape. blob may be empty (the endpoint can answer a reasoning item with
// no encrypted content); callers that resend reasoning must drop such a block
// rather than send it, which buildResponsesRequest does.
func ParseResponsesSignature(sig string) (id, blob string, ok bool) {
	rest, found := strings.CutPrefix(sig, ResponsesSignaturePrefix)
	if !found {
		return "", "", false
	}
	id, blob, found = strings.Cut(rest, responsesSignatureSeparator)
	if !found || id == "" {
		return "", "", false
	}
	return id, blob, true
}

// StripForeignThinking returns a copy of msgs keeping only thinking blocks whose
// signature belongs to the target family: forResponses=true keeps only Responses
// signatures, false keeps only the others. Other blocks are untouched, and the
// caller's slice is never mutated. A message left with no content keeps an empty
// content slice (callers already tolerate it).
//
// Both directions of provider failover need this. A conversation started on
// OpenAI Responses and continued on Anthropic carries reasoning items whose
// signatures Anthropic cannot verify; a conversation started on Anthropic and
// retried on OpenAI carries signatures that are not item ids at all. Neither
// side rejects the request cleanly: Anthropic errors on a signature it did not
// mint, and OpenAI treats an unknown reasoning id as nothing to resume, which
// silently loses the reasoning rather than failing. Dropping the foreign block
// is the one behavior both sides agree on.
//
// Shared by sender failover (both senders below) and conversation cloning, which
// is why it is exported and takes no receiver.
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
			if b.OfThinking != nil {
				mine := IsResponsesSignature(b.OfThinking.Signature)
				if mine != forResponses {
					continue
				}
			}
			kept = append(kept, b)
		}
		out[i].Content = kept
	}
	return out
}
