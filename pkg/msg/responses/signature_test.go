package responses_test

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg/responses"
)

// TestSignatureRoundTripByteIdentical pins that a signature survives a parse and
// a re-emit unchanged: the item id and the encrypted blob are opaque to rafiki,
// so the only thing that may change between minting and resending is nothing at
// all.
func TestSignatureRoundTripByteIdentical(t *testing.T) {
	for _, mode := range []responses.Mode{responses.ModeAPI, responses.ModeChatGPT} {
		t.Run(string(mode), func(t *testing.T) {
			ck := assert.NewAborting(t)
			const (
				id = "rs_1"
				// A colon proves the blob is cut only once, past the id, and is then
				// reproduced verbatim.
				blob = "gAAAA:a-blob"
			)
			sig := responses.Signature(mode, id, blob)
			gotMode, gotID, gotBlob, ok := responses.ParseSignature(sig)
			ck.True(ok, "ParseSignature(%q)", sig)
			ck.Eq(mode, gotMode, "mode")
			ck.Eq(id, gotID, "id")
			ck.Eq(blob, gotBlob, "blob")
			ck.Eq(sig, responses.Signature(gotMode, gotID, gotBlob), "re-emitted signature")
			ck.True(responses.IsSignature(sig), "IsSignature(%q)", sig)
		})
	}
}

// TestParseSignatureRejectsTwoFieldShape pins the shapes this codec refuses. The
// older two-field form names no backend, so a blob carrying it could not be
// resumed anywhere and must not be mistaken for one that can.
func TestParseSignatureRejectsTwoFieldShape(t *testing.T) {
	cases := []string{
		"openai-responses:rs_1:blob",
		"openai-responses:",
		"openai-responses:api:",
		"openai-responses:api",
		"openai-responses:api:rs_1",
		"openai-responses:other:rs_1:blob",
		"openai-responses::rs_1:blob",
		"anthropic:rs_1:blob",
		"",
	}
	for _, sig := range cases {
		t.Run(sig, func(t *testing.T) {
			ck := assert.NewAborting(t)
			mode, id, blob, ok := responses.ParseSignature(sig)
			ck.False(ok, "ParseSignature(%q) must not parse", sig)
			ck.Eq(responses.Mode(""), mode, "mode")
			ck.Eq("", id, "id")
			ck.Eq("", blob, "blob")
		})
	}
}

// TestParseSignatureAcceptsEmptyBlob pins that a reasoning item with no encrypted
// content still parses: the signature is what marks the block as this codec's, and
// it is the caller that decides an empty blob is nothing to resend.
func TestParseSignatureAcceptsEmptyBlob(t *testing.T) {
	ck := assert.NewAborting(t)
	sig := responses.Signature(responses.ModeAPI, "rs_1", "")
	mode, id, blob, ok := responses.ParseSignature(sig)
	ck.True(ok, "ParseSignature(%q)", sig)
	ck.Eq(responses.ModeAPI, mode, "mode")
	ck.Eq("rs_1", id, "id")
	ck.Eq("", blob, "blob")
	ck.True(responses.IsSignature(sig), "IsSignature is mode-blind")
	ck.True(responses.IsSignature(responses.Signature(responses.ModeChatGPT, "rs_2", "x")), "IsSignature accepts either mode")
	ck.False(responses.IsSignature("sig-anthropic"), "an Anthropic signature is not a Responses one")
}
