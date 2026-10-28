// SPDX-License-Identifier: Apache-2.0

package secretbox_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/secretbox"

	"github.com/multigres/testkit/assert"
)

// keyMaterial builds a readable fixture key. No test here uses a real key.
func keyMaterial(fill byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	return key
}

// keyA and keyB are distinct fixture keys, 32 bytes each.
var (
	keyA = keyMaterial(0x41)
	keyB = keyMaterial(0x42)
)

// newBox builds a box from a known-good key, failing the test rather than
// returning a nil box.
func newBox(t *testing.T, key []byte) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(key)
	assert.NewAborting(t).NoError(err, "New of a 32-byte key")
	return box
}

// TestSealOpenRoundTrip pins the output layout (nonce, then ciphertext and tag)
// and that a box opens its own seal under the same AAD.
func TestSealOpenRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	box := newBox(t, keyA)
	plaintext := []byte("refresh-token-value")

	sealed := box.Seal(plaintext, []byte("u1|openai|refresh"))
	c.Len(sealed, 12+len(plaintext)+16, "nonce(12) || ciphertext || tag(16)")
	c.False(bytes.Contains(sealed, plaintext), "the plaintext must not appear in the ciphertext")

	got, err := box.Open(box.KeyID(), sealed, []byte("u1|openai|refresh"))
	c.NoError(err, "Open of a ciphertext this box sealed")
	c.Eq(string(plaintext), string(got), "round trip")
}

// TestOpenRejectsSwappedAAD pins that the associated data binds a ciphertext to
// its row: the right AAD opens, and any other owner, provider or field fails.
func TestOpenRejectsSwappedAAD(t *testing.T) {
	c := assert.NewAborting(t)
	box := newBox(t, keyA)
	sealed := box.Seal([]byte("access-token"), []byte("u1|p|refresh"))

	got, err := box.Open(box.KeyID(), sealed, []byte("u1|p|refresh"))
	c.NoError(err, "the AAD it was sealed with must open it, else the rows below prove nothing")
	c.Eq("access-token", string(got), "round trip under the original AAD")

	for _, aad := range []string{"u2|p|refresh", "u1|q|refresh", "u1|p|access"} {
		got, err := box.Open(box.KeyID(), sealed, []byte(aad))
		c.ErrorIs(err, secretbox.ErrOpen, "AAD %q", aad)
		c.Empty(got, "nothing may be returned for AAD %q", aad)
	}
}

// TestOpenRejectsWrongKey pins the two distinct failures: another key's id is
// ErrUnknownKey (checked before any decryption, so even a truncated input gets
// that answer), while this box's own id on a foreign ciphertext is ErrOpen.
func TestOpenRejectsWrongKey(t *testing.T) {
	c := assert.NewAborting(t)
	boxA := newBox(t, keyA)
	boxB := newBox(t, keyB)
	c.NotEq(boxA.KeyID(), boxB.KeyID(), "two different keys must have different ids")

	aad := []byte("u1|p|refresh")
	sealedA := boxA.Seal([]byte("access-token"), aad)

	got, err := boxB.Open(boxA.KeyID(), sealedA, aad)
	c.ErrorIs(err, secretbox.ErrUnknownKey, "a foreign key id is ErrUnknownKey")
	c.Empty(got, "nothing may be returned under a foreign key id")

	got, err = boxB.Open(boxA.KeyID(), sealedA[:5], aad)
	c.ErrorIs(err, secretbox.ErrUnknownKey, "the key id is checked before the length")
	c.Empty(got, "nothing may be returned under a foreign key id")

	got, err = boxB.Open(boxB.KeyID(), sealedA, aad)
	c.ErrorIs(err, secretbox.ErrOpen, "this key's id on a foreign ciphertext is ErrOpen")
	c.Empty(got, "nothing may be returned for a failed authentication")
}

// TestOpenRejectsTruncated pins that short input is ErrOpen and not a panic,
// including one byte short of the smallest possible seal.
func TestOpenRejectsTruncated(t *testing.T) {
	c := assert.NewAborting(t)
	box := newBox(t, keyA)
	aad := []byte("u1|p|refresh")
	sealed := box.Seal([]byte("x"), aad)

	for _, cut := range []int{0, 1, 5, 12, 27, len(sealed) - 1} {
		var (
			got []byte
			err error
		)
		c.NotPanics(func() { got, err = box.Open(box.KeyID(), sealed[:cut], aad) }, "Open of %d bytes", cut)
		c.ErrorIs(err, secretbox.ErrOpen, "Open of %d bytes", cut)
		c.Empty(got, "nothing may be returned for %d bytes", cut)
	}

	got, err := box.Open(box.KeyID(), nil, aad)
	c.ErrorIs(err, secretbox.ErrOpen, "nil input")
	c.Empty(got, "nothing may be returned for nil input")
}

// TestSealUsesFreshNonce pins that every Seal draws its own nonce: a repeated
// nonce under one key would break GCM outright.
func TestSealUsesFreshNonce(t *testing.T) {
	c := assert.NewAborting(t)
	box := newBox(t, keyA)
	aad := []byte("u1|p|refresh")

	first := box.Seal([]byte("same plaintext"), aad)
	second := box.Seal([]byte("same plaintext"), aad)
	c.False(bytes.Equal(first, second), "two seals of one plaintext must differ")
	c.False(bytes.Equal(first[:12], second[:12]), "the nonces must differ, not just the ciphertext")
}

// TestFromEnv pins the environment contract: unset and empty are ErrNoKey, and
// anything that is not base64 of exactly 32 bytes is ErrBadKey.
func TestFromEnv(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(keyA)
	tests := []struct {
		name  string
		value string
		unset bool
		want  error
	}{
		{name: "unset", unset: true, want: secretbox.ErrNoKey},
		{name: "empty", value: "", want: secretbox.ErrNoKey},
		{name: "not base64", value: "not base64!", want: secretbox.ErrBadKey},
		{name: "31 bytes", value: base64.StdEncoding.EncodeToString(keyA[:31]), want: secretbox.ErrBadKey},
		{name: "33 bytes", value: base64.StdEncoding.EncodeToString(append(append([]byte{}, keyA...), 0)), want: secretbox.ErrBadKey},
		{name: "32 bytes", value: valid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			t.Setenv(secretbox.EnvKey, tt.value)
			if tt.unset {
				c.NoError(os.Unsetenv(secretbox.EnvKey), "clearing %s", secretbox.EnvKey)
			}
			box, err := secretbox.FromEnv()
			if tt.want != nil {
				c.ErrorIs(err, tt.want, "FromEnv with %s=%q", secretbox.EnvKey, tt.value)
				c.True(box == nil, "a failed FromEnv must not return a box")
				return
			}
			c.NoError(err, "FromEnv with a valid key")
			c.Eq(newBox(t, keyA).KeyID(), box.KeyID(), "the id of the key the variable holds")
			sealed := box.Seal([]byte("access-token"), []byte("u1|p|refresh"))
			got, err := box.Open(box.KeyID(), sealed, []byte("u1|p|refresh"))
			c.NoError(err, "a box from the environment must open what it sealed")
			c.Eq("access-token", string(got), "round trip through FromEnv")
		})
	}
}

// TestKeyIDStable pins the id's shape and that it follows the key, since a
// rotation depends on two keys never sharing one.
func TestKeyIDStable(t *testing.T) {
	c := assert.NewAborting(t)

	first := newBox(t, keyA)
	second := newBox(t, keyA)
	c.Eq(first.KeyID(), second.KeyID(), "the same key must give the same id")
	c.Len(first.KeyID(), 16, "8 bytes of sha256 in hex")
	for _, r := range first.KeyID() {
		c.True(strings.ContainsRune("0123456789abcdef", r), "id %q must be lowercase hex, found %q", first.KeyID(), r)
	}
	c.NotEq(newBox(t, keyB).KeyID(), first.KeyID(), "a different key must give a different id")
}

// TestGenerateKey pins what setup writes into the environment: std base64 of
// exactly 32 bytes, and a different key every call.
func TestGenerateKey(t *testing.T) {
	c := assert.NewAborting(t)

	first, err := secretbox.GenerateKey()
	c.NoError(err, "GenerateKey")
	key, err := base64.StdEncoding.DecodeString(first)
	c.NoError(err, "the generated key must be std base64")
	c.Len(key, 32, "a 32-byte key")

	second, err := secretbox.GenerateKey()
	c.NoError(err, "GenerateKey")
	c.NotEq(first, second, "two generated keys must differ")

	box, err := secretbox.New(key)
	c.NoError(err, "a generated key must be usable by New")
	sealed := box.Seal([]byte("access-token"), []byte("u1|p|refresh"))
	got, err := box.Open(box.KeyID(), sealed, []byte("u1|p|refresh"))
	c.NoError(err, "a generated key must open what it sealed")
	c.Eq("access-token", string(got), "round trip with a generated key")
}

// TestErrorStringsAreStable pins the four sentinel values verbatim: later waves
// branch on ErrUnknownKey to schedule a re-seal, and an operator reads the rest.
func TestErrorStringsAreStable(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("RAFIKI_SECRETS_KEY", secretbox.EnvKey, "secretbox.EnvKey")
	c.Eq("secretbox: RAFIKI_SECRETS_KEY is not set", secretbox.ErrNoKey.Error(), "ErrNoKey")
	c.Eq("secretbox: RAFIKI_SECRETS_KEY must be base64 of exactly 32 bytes", secretbox.ErrBadKey.Error(), "ErrBadKey")
	c.Eq("secretbox: ciphertext was sealed under a different key", secretbox.ErrUnknownKey.Error(), "ErrUnknownKey")
	c.Eq("secretbox: ciphertext failed authentication", secretbox.ErrOpen.Error(), "ErrOpen")
}
