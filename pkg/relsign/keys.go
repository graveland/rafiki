// SPDX-License-Identifier: Apache-2.0

package relsign

import (
	"crypto/ed25519"
	"encoding/base64"
)

// TrustedKeys is the release trust anchor: the keys `rafiki upgrade` accepts
// signatures from. Keys are never removed; a retired key gets a MaxVersion.
// The single entry below is the current key and is unbounded.
var TrustedKeys = []Key{
	{ID: "403f6dd4", PublicKey: mustKey("BdmwhW18EF9jfnKxF9yxSnhK1MsCF86emPd262Gpyck=")},
}

// mustKey decodes a base64 public key constant, panicking on a malformed one:
// an init-time failure beats a silently untrusted key.
func mustKey(encoded string) ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("relsign: malformed trusted public key: " + err.Error())
	}
	return ed25519.PublicKey(raw)
}
