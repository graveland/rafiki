// SPDX-License-Identifier: Apache-2.0

package relsign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Key is one trusted release-signing key.
type Key struct {
	ID         string // KeyID(PublicKey)
	PublicKey  ed25519.PublicKey
	MaxVersion string // "" = no bound; else the last tag (vX.Y.Z) the key may sign
}

var (
	ErrMalformedSignature = errors.New("relsign: malformed signature file")
	ErrUnknownKey         = errors.New("relsign: signature key is not trusted")
	ErrBadSignature       = errors.New("relsign: signature does not match")
	ErrKeyNotValidForTag  = errors.New("relsign: key is not valid for this release")
)

// KeyID is the first 8 hex characters of sha256 of the raw 32-byte public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:8]
}

// GenerateKey returns a fresh 32-byte seed and its public key.
func GenerateKey() ([]byte, ed25519.PublicKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, nil, fmt.Errorf("relsign: generate seed: %w", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return seed, pub, nil
}

// PrivateFromSeed returns the private key for a 32-byte seed; any other length is an error.
func PrivateFromSeed(seed []byte) (ed25519.PrivateKey, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("relsign: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Sign returns the contents of a .sig file for message: "<KeyID> <std base64 signature>\n".
func Sign(priv ed25519.PrivateKey, message []byte) []byte {
	sig := ed25519.Sign(priv, message)
	id := KeyID(priv.Public().(ed25519.PublicKey))
	return []byte(id + " " + base64.StdEncoding.EncodeToString(sig) + "\n")
}

// Verify returns nil only when sigFile is one well-formed line naming a key in
// keys, that key's signature over exactly message verifies, targetTag is a
// release tag, and the key's MaxVersion (when set) is not below targetTag.
func Verify(keys []Key, message, sigFile []byte, targetTag string) error {
	line := strings.TrimSpace(string(sigFile))
	if line == "" || strings.Contains(line, "\n") {
		return fmt.Errorf("%w: want exactly one line", ErrMalformedSignature)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return fmt.Errorf("%w: want 2 fields, got %d", ErrMalformedSignature, len(fields))
	}

	sig, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return fmt.Errorf("%w: signature is not base64: %v", ErrMalformedSignature, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature is %d bytes, want %d", ErrMalformedSignature, len(sig), ed25519.SignatureSize)
	}

	var key *Key
	for i := range keys {
		if keys[i].ID == fields[0] {
			key = &keys[i]
			break
		}
	}
	if key == nil {
		return fmt.Errorf("%w: %s", ErrUnknownKey, fields[0])
	}

	if len(key.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key is %d bytes, want %d", ErrBadSignature, len(key.PublicKey), ed25519.PublicKeySize)
	}
	if !ed25519.Verify(key.PublicKey, message, sig) {
		return fmt.Errorf("%w: %s", ErrBadSignature, fields[0])
	}

	if _, err := ParseTag(targetTag); err != nil {
		return err
	}
	if key.MaxVersion != "" {
		cmp, err := CompareTags(targetTag, key.MaxVersion)
		if err != nil {
			return err
		}
		if cmp > 0 {
			return fmt.Errorf("%w: key ends at %s", ErrKeyNotValidForTag, key.MaxVersion)
		}
	}
	return nil
}
