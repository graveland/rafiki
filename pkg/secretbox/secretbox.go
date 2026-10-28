// SPDX-License-Identifier: Apache-2.0

// Package secretbox seals small secrets at rest with AES-256-GCM.
//
// The key is a deployment secret, supplied through EnvKey (base64 of exactly 32
// bytes). It is never written down here and never stored beside the data it
// protects: losing it loses only what was sealed with it, which is the correct
// outcome for credentials that can be re-acquired, and the failure is loud
// (ErrNoKey, ErrBadKey) rather than a plaintext fallback.
//
// Every box also carries a key id, a truncated hash of its key. Ciphertext is
// expected to be stored alongside the id that sealed it, so a later key
// rotation can tell "sealed under another key" (ErrUnknownKey) apart from
// "corrupt or tampered with" (ErrOpen) and re-seal at leisure instead of
// needing a flag day in which every row is rewritten at once.
//
// Seal takes associated data, which is authenticated but not encrypted. Callers
// bind a ciphertext to its position in the schema (for example the owner, the
// provider and the field) so that a row copied to another owner, provider or
// field fails to open rather than yielding someone else's credential.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// EnvKey is the environment variable holding the base64 (std encoding) of a
// 32-byte key.
const EnvKey = "RAFIKI_SECRETS_KEY"

var (
	// ErrNoKey is returned when the key variable is unset or empty.
	ErrNoKey = errors.New("secretbox: RAFIKI_SECRETS_KEY is not set")
	// ErrBadKey is returned when the key is not base64 of exactly 32 bytes.
	ErrBadKey = errors.New("secretbox: RAFIKI_SECRETS_KEY must be base64 of exactly 32 bytes")
	// ErrUnknownKey is returned when a ciphertext was sealed by a box whose
	// key id differs from this one's. It is a rotation signal, not corruption.
	ErrUnknownKey = errors.New("secretbox: ciphertext was sealed under a different key")
	// ErrOpen is returned when a ciphertext is too short to hold a nonce and a
	// tag, or fails authentication.
	ErrOpen = errors.New("secretbox: ciphertext failed authentication")
)

const (
	// keyLen is the AES-256 key size; the one size this package accepts.
	keyLen = 32
	// nonceLen is the GCM standard nonce size. Seal writes one fresh nonce per
	// call at the front of its output, so nothing else has to carry it.
	nonceLen = 12
	// keyIDLen is how many bytes of the key's sha256 the id keeps. 8 bytes (16
	// hex characters) is far more than enough to tell two live keys apart, and
	// it is an identifier, never a secret.
	keyIDLen = 8
)

// Box seals and opens with one key. It is safe for concurrent use; the
// underlying GCM cipher is stateless once built.
type Box struct {
	aead cipher.AEAD
	id   string
}

// New builds a Box from a raw 32-byte key. Any other length is ErrBadKey: a
// short key would silently select AES-128 or AES-192, which is not what a
// caller asking for this package means.
func New(key []byte) (*Box, error) {
	if len(key) != keyLen {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadKey, err)
	}
	if nonceLen != aead.NonceSize() {
		return nil, fmt.Errorf("%w: unexpected GCM nonce size %d", ErrBadKey, aead.NonceSize())
	}
	sum := sha256.Sum256(key)
	return &Box{aead: aead, id: fmt.Sprintf("%x", sum[:keyIDLen])}, nil
}

// FromEnv builds a Box from EnvKey. Unset or empty is ErrNoKey; anything that
// is not base64 of exactly 32 bytes is ErrBadKey.
func FromEnv() (*Box, error) {
	raw := os.Getenv(EnvKey)
	if raw == "" {
		return nil, ErrNoKey
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != keyLen {
		return nil, ErrBadKey
	}
	return New(key)
}

// GenerateKey returns a fresh base64 (std encoding) 32-byte key, for setup to
// put in EnvKey. It is the only thing here that hands out key material.
func GenerateKey() (string, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("secretbox: generating key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// KeyID is the 16 hex character identifier of this box's key, to be stored
// beside the ciphertext it sealed.
func (b *Box) KeyID() string { return b.id }

// Seal encrypts plaintext under a fresh random nonce and returns
// nonce(12) || ciphertext||tag. aad is authenticated but not encrypted; pass
// the identity of the row the secret belongs to.
//
// Seal panics if the platform random source fails. Returning a ciphertext built
// without a fresh nonce would silently destroy the one property GCM depends on,
// so this is a stop-everything condition, and crypto/rand does not fail on any
// platform this daemon runs on.
func (b *Box) Seal(plaintext, aad []byte) []byte {
	out := make([]byte, nonceLen, nonceLen+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		panic("secretbox: crypto/rand failed: " + err.Error())
	}
	return b.aead.Seal(out, out[:nonceLen], plaintext, aad)
}

// Open reverses Seal for a ciphertext sealed under the key identified by keyID.
//
// A keyID that is not this box's is ErrUnknownKey and nothing is decrypted, so
// a rotation wiring bug reads as "wrong key" and never as corruption. Short
// input and authentication failure are both ErrOpen.
//
// Neither error carries plaintext or key material, and neither does any error
// in this package.
func (b *Box) Open(keyID string, sealed, aad []byte) ([]byte, error) {
	if keyID != b.id {
		return nil, ErrUnknownKey
	}
	if len(sealed) < nonceLen+b.aead.Overhead() {
		return nil, ErrOpen
	}
	plaintext, err := b.aead.Open(nil, sealed[:nonceLen], sealed[nonceLen:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}
