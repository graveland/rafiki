// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ResponsesSignaturePrefix starts every thinking signature this package writes
// for an OpenAI Responses reasoning item. The whole signature is
// "<prefix><mode>:<item id>:<encrypted content>": the dialect that minted the
// blob, the item id the endpoint needs to accept a resent reasoning item, and
// the opaque blob it encrypts the model's reasoning into.
//
// Why a signature at all: an Anthropic thinking block carries exactly two
// strings, thinking and signature, and rafiki stores a turn as Anthropic
// shapes whatever provider produced it. The signature is the only field with
// no meaning of its own, so it is where a foreign provider's continuation
// state rides. On the way back out, the prefix is what tells a provider whose
// state it is: a reasoning item resent to OpenAI as a thinking block, or a
// Claude signature sent to OpenAI as a reasoning id, is at best rejected and at
// worst silently ignored.
//
// Why the mode as well as the prefix: the encrypted content is bound to the
// backend that produced it, and this package talks to two Responses backends.
// A ChatGPT-minted blob sent to api.openai.com (or a compatible endpoint) is
// no more verifiable than an Anthropic signature is to ChatGPT, so the
// signature has to name which backend may resume it.
const ResponsesSignaturePrefix = "openai-responses:"

// responsesSignatureSeparator joins the mode, the item id and the encrypted
// blob inside a signature. The blob is base64url and never contains it; a blob
// that somehow did would keep its colons (ParseResponsesSignature cuts once
// past the id).
const responsesSignatureSeparator = ":"

// ResponsesSignatureMode names the Responses backend dialect that minted a
// thinking signature's encrypted content. It is a closed set: a signature
// naming any other mode does not parse, because there is no backend here that
// could resume such a blob.
type ResponsesSignatureMode string

const (
	// ResponsesModeAPI is the plain openai-responses kind: api.openai.com under
	// an operator's key, or a compatible endpoint (a local vLLM via_executor).
	ResponsesModeAPI ResponsesSignatureMode = "api"
	// ResponsesModeChatGPT is the openai-responses-chatgpt subscription dialect,
	// whose blobs only the ChatGPT backend accepts.
	ResponsesModeChatGPT ResponsesSignatureMode = "chatgpt"
)

// valid reports whether m is a mode this package writes. An unknown mode is
// refused at parse time rather than kept as an opaque label: a blob under a
// mode nobody can resume is dead weight.
func (m ResponsesSignatureMode) valid() bool {
	return m == ResponsesModeAPI || m == ResponsesModeChatGPT
}

// responsesSignature renders the thinking signature for one reasoning item
// minted by mode.
func responsesSignature(mode ResponsesSignatureMode, itemID, encryptedContent string) string {
	return ResponsesSignaturePrefix + string(mode) + responsesSignatureSeparator +
		itemID + responsesSignatureSeparator + encryptedContent
}

// IsResponsesSignature reports whether a thinking signature was produced by a
// Responses sender, whichever mode minted it. Deliberately mode-blind: the
// Anthropic direction must drop every Responses signature, because Anthropic
// can verify none of them.
func IsResponsesSignature(sig string) bool {
	return strings.HasPrefix(sig, ResponsesSignaturePrefix)
}

// ParseResponsesSignature splits "openai-responses:<mode>:<id>:<blob>";
// ok=false on any other shape, including the older two-field
// "<prefix><id>:<blob>" form and an unknown mode. blob may be empty (the
// endpoint can answer a reasoning item with no encrypted content); callers that
// resend reasoning must drop such a block rather than send it, which
// buildResponsesRequestBody does.
func ParseResponsesSignature(sig string) (mode ResponsesSignatureMode, id, blob string, ok bool) {
	rest, found := strings.CutPrefix(sig, ResponsesSignaturePrefix)
	if !found {
		return "", "", "", false
	}
	rawMode, rest, found := strings.Cut(rest, responsesSignatureSeparator)
	if !found {
		return "", "", "", false
	}
	mode = ResponsesSignatureMode(rawMode)
	if !mode.valid() {
		return "", "", "", false
	}
	id, blob, found = strings.Cut(rest, responsesSignatureSeparator)
	if !found || id == "" {
		return "", "", "", false
	}
	return mode, id, blob, true
}

// StripForeignThinking returns a copy of msgs keeping only thinking blocks whose
// signature belongs to the target family: forResponses=true keeps every
// Responses signature (whichever mode), false keeps only the others. Other
// blocks are untouched, and the caller's slice is never mutated. A message left
// with no content keeps an empty content slice (callers already tolerate it).
//
// This is the family-level filter, used by the Anthropic senders and by
// conversation cloning. A Responses sender resends one DIALECT only, so it uses
// StripForeignResponsesThinking instead: an api-mode blob means nothing to the
// ChatGPT backend and vice versa.
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
// Stripping is not free: a message whose only non-text block was foreign can be
// left with no thinking block at all, and an extended-thinking Anthropic provider
// may reject an assistant turn that carries text but no thinking after one that
// had it. That consequence is accepted by the design: a turn that cannot resume
// its reasoning is still a turn that can continue, where a signature the provider
// rejects fails the request outright.
//
// Shared by sender failover (both senders below) and conversation cloning, which
// is why it is exported and takes no receiver.
func StripForeignThinking(msgs []anthropic.MessageParam, forResponses bool) []anthropic.MessageParam {
	if forResponses {
		return stripThinking(msgs, IsResponsesSignature)
	}
	return stripThinking(msgs, func(sig string) bool { return !IsResponsesSignature(sig) })
}

// StripForeignResponsesThinking returns a copy of msgs keeping only Responses
// thinking blocks whose signature was minted by mode. A block from the other
// Responses dialect is dropped, and so is every non-Responses signature: a
// blob's encrypted content is only meaningful to the backend that produced it,
// which is the design's cross-provider rule applied one level finer than the
// family split.
//
// Unlike StripForeignThinking this doubles as the parse gate: a Responses-prefixed
// signature that does not parse names no item, so there is nothing to resend.
func StripForeignResponsesThinking(msgs []anthropic.MessageParam, mode ResponsesSignatureMode) []anthropic.MessageParam {
	return stripThinking(msgs, func(sig string) bool {
		minted, _, _, ok := ParseResponsesSignature(sig)
		return ok && minted == mode
	})
}

// stripThinking copies msgs keeping only thinking blocks whose signature keep
// accepts, and drops nothing else.
func stripThinking(msgs []anthropic.MessageParam, keep func(signature string) bool) []anthropic.MessageParam {
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
			if b.OfThinking != nil && !keep(b.OfThinking.Signature) {
				continue
			}
			kept = append(kept, b)
		}
		out[i].Content = kept
	}
	return out
}
