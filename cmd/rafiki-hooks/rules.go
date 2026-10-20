// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
)

// lookup reads a dotted path out of a decoded JSON payload, where a numeric
// segment indexes an array.
//
// Only scalars have a text form a notification can carry: strings are returned
// as-is, numbers and booleans in their JSON text form, and an object or array
// is reported absent rather than flattened into something no reader expects.
func lookup(payload any, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	current := payload
	for _, segment := range strings.Split(path, ".") {
		switch node := current.(type) {
		case map[string]any:
			value, ok := node[segment]
			if !ok {
				return "", false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return "", false
			}
			current = node[index]
		default:
			return "", false
		}
	}
	switch value := current.(type) {
	case string:
		return value, true
	case json.Number:
		// The literal text the sender wrote: 1.50 stays 1.50.
		return value.String(), true
	case float64:
		// A payload decoded without UseNumber. json.Marshal is what "the JSON
		// text form" means for a float, including its exponent spelling.
		text, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		return string(text), true
	case bool:
		return strconv.FormatBool(value), true
	}
	return "", false
}

// firstMatch returns the first rule that claims the delivery. Order is the
// whole point of rules being a list: the first match wins, so a specific rule
// placed above a catch-all is how a sender's noisy events get carved out.
func firstMatch(rules []Rule, eventType string, payload any) (Rule, bool) {
	for _, rule := range rules {
		if rule.matches(eventType, payload) {
			return rule, true
		}
	}
	return Rule{}, false
}

// matches reports whether this rule claims the delivery: the event must agree
// (or the rule must accept any event) and every match entry must hold.
func (r Rule) matches(eventType string, payload any) bool {
	if r.Event != "" && r.Event != eventType {
		return false
	}
	for path, want := range r.Match {
		got, ok := lookup(payload, path)
		if !ok || got != want {
			return false
		}
	}
	return true
}

// build renders the notification: the key template interpolated, and each field
// read from the payload. A path that is not there renders as the empty string
// and is logged at debug, because a sender adding or dropping a field should
// degrade the message's detail, never the delivery.
func (r Rule) build(payload any) (key string, fields map[string]string) {
	key = interpolate(r.Key, payload)
	fields = make(map[string]string, len(r.Fields))
	for name, path := range r.Fields {
		value, ok := lookup(payload, path)
		if !ok {
			slog.Debug("rafiki-hooks: field path is absent from the payload", "field", name, "path", path)
		}
		fields[name] = value
	}
	return key, fields
}

// interpolate substitutes {dotted.path} in a key template. Text outside braces
// is copied verbatim, and a path that is absent (or a stray brace) leaves the
// hole empty rather than failing the delivery.
func interpolate(template string, payload any) string {
	var out strings.Builder
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			out.WriteString(rest)
			return out.String()
		}
		end := strings.IndexByte(rest[open+1:], '}')
		if end < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:open])
		if value, ok := lookup(payload, rest[open+1:open+1+end]); ok {
			out.WriteString(value)
		}
		rest = rest[open+1+end+1:]
	}
}
