// SPDX-License-Identifier: Apache-2.0

package relsign

// TrustedKeys is the release trust anchor: the keys `rafiki upgrade` accepts
// signatures from. Keys are never removed; a retired key gets a MaxVersion.
var TrustedKeys = []Key{}
