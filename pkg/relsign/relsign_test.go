// SPDX-License-Identifier: Apache-2.0

package relsign_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/relsign"
)

func mustKey(t *testing.T, seed []byte) ed25519.PrivateKey {
	t.Helper()
	priv, err := relsign.PrivateFromSeed(seed)
	if err != nil {
		t.Fatalf("PrivateFromSeed: %v", err)
	}
	return priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	priv := mustKey(t, seed)

	msg := []byte("checksums\n")
	sig := relsign.Sign(priv, msg)
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}}
	c.NoError(relsign.Verify(keys, msg, sig, "v0.2.0"), "Verify()")
}

func TestVerifyRejectsTamperedMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	priv := mustKey(t, seed)

	sig := relsign.Sign(priv, []byte("original"))
	tampered := []byte("originaL")
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}}
	c.ErrorIs(relsign.Verify(keys, tampered, sig, "v0.2.0"), relsign.ErrBadSignature, "Verify()")
}

func TestVerifyUnknownKeyID(t *testing.T) {
	c := assert.NewCollecting(t)
	seedA, _, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey() A")
	_, pubB, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey() B")

	sig := relsign.Sign(mustKey(t, seedA), []byte("msg"))
	keys := []relsign.Key{{ID: relsign.KeyID(pubB), PublicKey: pubB}}
	c.ErrorIs(relsign.Verify(keys, []byte("msg"), sig, "v0.2.0"), relsign.ErrUnknownKey, "Verify()")
}

func TestVerifyRejectsSignatureFromAnotherKeyUnderTrustedID(t *testing.T) {
	c := assert.NewCollecting(t)
	seedA, _, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey() A")
	_, pubB, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey() B")

	fields := strings.Fields(string(relsign.Sign(mustKey(t, seedA), []byte("msg"))))
	fields[0] = relsign.KeyID(pubB)
	rewritten := []byte(strings.Join(fields, " ") + "\n")

	keys := []relsign.Key{{ID: relsign.KeyID(pubB), PublicKey: pubB}}
	c.ErrorIs(relsign.Verify(keys, []byte("msg"), rewritten, "v0.2.0"), relsign.ErrBadSignature, "Verify()")
}

func TestVerifyMalformedSignatureFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	priv := mustKey(t, seed)
	msg := []byte("msg")
	validSig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}}

	cases := map[string][]byte{
		"empty":            {},
		"whitespace only":  []byte("  \t\n "),
		"one field":        []byte("abcd\n"),
		"three fields":     []byte("abcd " + validSig + " extra\n"),
		"two lines":        []byte("abcd " + validSig + "\nabcd " + validSig + "\n"),
		"non-base64":       []byte("abcd !!!not-base64!!!\n"),
		"base64 of 63":     []byte("abcd " + base64.StdEncoding.EncodeToString(make([]byte, 63)) + "\n"),
		"base64 of 65":     []byte("abcd " + base64.StdEncoding.EncodeToString(make([]byte, 65)) + "\n"),
		"trailing garbage": []byte("abcd " + validSig + " !!bad\n"),
	}
	for name, sigFile := range cases {
		c.ErrorIs(relsign.Verify(keys, msg, sigFile, "v0.2.0"), relsign.ErrMalformedSignature, "Verify() %s", name)
	}
}

func TestVerifyMaxVersionBounds(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	priv := mustKey(t, seed)
	msg := []byte("msg")
	sig := relsign.Sign(priv, msg)

	id := relsign.KeyID(pub)
	bounded := []relsign.Key{{ID: id, PublicKey: pub, MaxVersion: "v0.3.0"}}
	unbounded := []relsign.Key{{ID: id, PublicKey: pub}}

	cases := []struct {
		name string
		keys []relsign.Key
		tag  string
		want error
	}{
		{"at bound", bounded, "v0.3.0", nil},
		{"below bound", bounded, "v0.2.9", nil},
		{"patch above bound", bounded, "v0.3.1", relsign.ErrKeyNotValidForTag},
		{"major above bound", bounded, "v1.0.0", relsign.ErrKeyNotValidForTag},
		{"unbounded accepts high tag", unbounded, "v9.9.9", nil},
	}
	for _, tc := range cases {
		err := relsign.Verify(tc.keys, msg, sig, tc.tag)
		if tc.want == nil {
			c.NoError(err, "Verify() %s", tc.name)
			continue
		}
		c.ErrorIs(err, tc.want, "Verify() %s", tc.name)
	}
}

func TestVerifyRejectsNonTagEvenWithUnboundedKey(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	msg := []byte("msg")
	sig := relsign.Sign(mustKey(t, seed), msg)
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}}

	for _, tag := range []string{"main", "0.3.0", "v1.2", ""} {
		c.ErrorIs(relsign.Verify(keys, msg, sig, tag), relsign.ErrBadTag, "Verify() tag %q", tag)
	}
}

func TestVerifyShortPublicKeyDoesNotPanic(t *testing.T) {
	c := assert.NewCollecting(t)
	pub := ed25519.PublicKey("12345")
	sigFile := []byte(relsign.KeyID(pub) + " " + base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)) + "\n")
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}}

	c.ErrorIs(relsign.Verify(keys, []byte("msg"), sigFile, "v0.2.0"), relsign.ErrBadSignature, "Verify()")
}

func TestKeyIDMatchesIndependentHash(t *testing.T) {
	c := assert.NewCollecting(t)
	_, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")

	sum := sha256.Sum256(pub)
	want := hex.EncodeToString(sum[:])[:8]
	c.Eq(want, relsign.KeyID(pub), "KeyID()")
}

func TestGenerateKeyAndSeed(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, pub, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	c.Len(seed, ed25519.SeedSize, "seed length")

	priv, err := relsign.PrivateFromSeed(seed)
	c.Require().NoError(err, "PrivateFromSeed()")
	c.EqDeep(pub, priv.Public().(ed25519.PublicKey), "private key public part")

	_, err = relsign.PrivateFromSeed(seed[:31])
	c.Error(err, "PrivateFromSeed() with a 31-byte seed")
}
