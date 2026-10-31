package responses

import "strings"

// signaturePrefix starts every thinking signature this codec writes for a
// Responses reasoning item. The whole signature is
// "<prefix><mode>:<item id>:<encrypted content>": the dialect that minted the
// blob, the item id the endpoint needs to accept a resent reasoning item, and
// the opaque blob it encrypts the model's reasoning into.
//
// A thinking block has exactly two strings, so the signature — the field with no
// meaning of its own — is where a foreign provider's continuation state rides.
// On the way back out the prefix is what tells a sender whose state it is: a
// Claude signature sent to OpenAI as a reasoning id, or a reasoning item sent to
// Anthropic as a thinking signature, is at best rejected and at worst silently
// ignored.
const signaturePrefix = "openai-responses:"

// signatureSeparator joins the mode, the item id and the encrypted blob inside a
// signature. The blob is base64url and never contains it; a blob that somehow
// did would keep its colons, because ParseSignature cuts once past the id.
const signatureSeparator = ":"

// Signature renders the thinking signature for one reasoning item minted by
// mode.
func Signature(mode Mode, itemID, encryptedContent string) string {
	return signaturePrefix + string(mode) + signatureSeparator + itemID + signatureSeparator + encryptedContent
}

// IsSignature reports whether a thinking signature was produced by a Responses
// codec, whichever mode minted it. Deliberately mode-blind: the Anthropic
// direction must drop every Responses signature, because Anthropic can verify
// none of them.
func IsSignature(sig string) bool {
	return strings.HasPrefix(sig, signaturePrefix)
}

// ParseSignature splits "openai-responses:<mode>:<id>:<blob>"; ok is false for
// any other shape, including the older two-field "<prefix><id>:<blob>" form and
// an unknown mode. blob may be empty — the endpoint can answer a reasoning item
// with no encrypted content — and a caller that resends reasoning must drop such
// a block rather than send it, since an empty blob carries nothing.
func ParseSignature(sig string) (mode Mode, id, blob string, ok bool) {
	rest, found := strings.CutPrefix(sig, signaturePrefix)
	if !found {
		return "", "", "", false
	}
	rawMode, rest, found := strings.Cut(rest, signatureSeparator)
	if !found {
		return "", "", "", false
	}
	mode = Mode(rawMode)
	if !mode.valid() {
		return "", "", "", false
	}
	id, blob, found = strings.Cut(rest, signatureSeparator)
	if !found || id == "" {
		return "", "", "", false
	}
	return mode, id, blob, true
}
