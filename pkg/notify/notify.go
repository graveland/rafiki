// SPDX-License-Identifier: Apache-2.0

// Package notify validates and renders the notification fragment the notify
// pipeline injects into an agent's prompt.
//
// Notification content is third-party data. This package guarantees it cannot
// break the frame's structure: the opening element, the body and the closing
// element stay three well-formed pieces whatever a caller supplies. It does
// NOT guarantee that a model ignores instructions inside the content — an
// agent can still read a field value as a request, and nothing here can stop
// that.
//
// The package is stdlib-only on purpose: the gateway and the daemon both
// import it, and the pgx-free client may too.
package notify

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxFields is the most fields one notification may carry.
	MaxFields = 32
	// MaxValueBytes is the largest single field value, in bytes.
	MaxValueBytes = 4096
	// MaxFieldsBytes is the largest encoded JSON object the fields may
	// produce, in bytes.
	MaxFieldsBytes = 16384
	// Origin is stamped on every fragment: the content came from a third
	// party, not from the operator or the agent.
	Origin = "third-party"
)

const (
	sourcePattern = `^[a-z0-9][a-z0-9._-]{0,63}$`
	keyPattern    = `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`
	fieldPattern  = `^[a-zA-Z0-9_.-]{1,64}$`
)

var (
	sourceRE = regexp.MustCompile(sourcePattern)
	keyRE    = regexp.MustCompile(keyPattern)
	fieldRE  = regexp.MustCompile(fieldPattern)
)

// maxEcho bounds how much offending input an error message repeats back. What
// reaches Validate is untrusted and unbounded, and a 10 MB source must not
// become a 10 MB error message.
const maxEcho = 64

// InvalidError reports a source, key or fields value that breaks one of the
// charsets or caps. Msg names the offending part.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// ValidSource reports whether source matches the charset a notification's
// source must satisfy, returning the same *InvalidError Validate produces for
// it. It is exported because the gateway validates a route's notify_source at
// startup and must enforce exactly the daemon's rule.
func ValidSource(source string) error {
	if !sourceRE.MatchString(source) {
		return &InvalidError{Msg: fmt.Sprintf("invalid source %s: must match %s", echo(source), sourcePattern)}
	}
	return nil
}

// Validate checks source, key and fields against the charsets and caps. Every
// failure is an *InvalidError naming the offending part. A nil fields map is
// valid and encodes as {}.
//
// Because fields is a map, when more than one field is invalid the field named
// in the error is undefined; Validate reports the first it reaches.
func Validate(source, key string, fields map[string]string) error {
	if err := ValidSource(source); err != nil {
		return err
	}
	if key != "" && !keyRE.MatchString(key) {
		return &InvalidError{Msg: fmt.Sprintf("invalid key %s: must be empty or match %s", echo(key), keyPattern)}
	}
	if len(fields) > MaxFields {
		return &InvalidError{Msg: fmt.Sprintf("too many fields: %d exceeds %d", len(fields), MaxFields)}
	}
	for name, value := range fields {
		if !fieldRE.MatchString(name) {
			return &InvalidError{Msg: fmt.Sprintf("invalid field name %s: must match %s", echo(name), fieldPattern)}
		}
		switch {
		case !utf8.ValidString(value):
			return &InvalidError{Msg: fmt.Sprintf("invalid value for field %s: not valid UTF-8", echo(name))}
		case strings.Contains(value, "\x00"):
			return &InvalidError{Msg: fmt.Sprintf("invalid value for field %s: contains a NUL byte", echo(name))}
		case len(value) > MaxValueBytes:
			return &InvalidError{Msg: fmt.Sprintf("value for field %s is %d bytes, over the %d byte limit", echo(name), len(value), MaxValueBytes)}
		}
	}
	encoded, err := encodeFields(fields)
	if err != nil {
		return &InvalidError{Msg: fmt.Sprintf("fields do not encode: %v", err)}
	}
	if len(encoded) > MaxFieldsBytes {
		return &InvalidError{Msg: fmt.Sprintf("encoded fields are %d bytes, over the %d byte limit", len(encoded), MaxFieldsBytes)}
	}
	return nil
}

// Render returns the notification fragment. It calls Validate first and
// returns its error unchanged; it never truncates, so a caller that gets an
// error gets no fragment at all.
func Render(source, key string, fields map[string]string, received time.Time) (string, error) {
	if err := Validate(source, key, fields); err != nil {
		return "", err
	}
	encoded, err := encodeFields(fields)
	if err != nil {
		return "", &InvalidError{Msg: fmt.Sprintf("fields do not encode: %v", err)}
	}
	// %q is safe here only because the charsets Validate enforces exclude '"'
	// and '\': source and key are checked by Validate, received is an RFC3339
	// stamp and Origin is a constant, so Go-quoting any of them cannot emit a
	// quote or a backslash that closes the attribute. Fields are the untrusted
	// free-form part, and they are interpolated only as encoding/json output,
	// which escapes '<', '>' and '&' and therefore cannot close the element.
	return fmt.Sprintf("<notification source=%q key=%q received=%q origin=%q>\n%s\n</notification>",
		source, key, received.UTC().Format(time.RFC3339), Origin, encoded), nil
}

// encodeFields marshals fields the way a fragment carries them: sorted keys,
// HTML-safe escaping, and {} rather than null for an empty or nil map.
func encodeFields(fields map[string]string) ([]byte, error) {
	if len(fields) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(fields)
}

// echo renders a bounded, quoted form of an offending value for an error
// message. Truncation may split a rune, so the cut is made valid again.
func echo(s string) string {
	if len(s) > maxEcho {
		return fmt.Sprintf("%q", strings.ToValidUTF8(s[:maxEcho], "")+"…")
	}
	return fmt.Sprintf("%q", s)
}
