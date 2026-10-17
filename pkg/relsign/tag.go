// SPDX-License-Identifier: Apache-2.0

package relsign

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrBadTag is returned for a string that is not a release tag vMAJOR.MINOR.PATCH.
var ErrBadTag = errors.New("relsign: not a release tag")

var tagRe = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseTag parses vMAJOR.MINOR.PATCH. Leading zeros, missing parts, a missing
// "v", pre-release suffixes and components that overflow uint64 are ErrBadTag.
func ParseTag(tag string) ([3]uint64, error) {
	m := tagRe.FindStringSubmatch(tag)
	if m == nil {
		return [3]uint64{}, fmt.Errorf("%w: %q", ErrBadTag, tag)
	}
	var out [3]uint64
	for i := range out {
		v, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return [3]uint64{}, fmt.Errorf("%w: %q", ErrBadTag, tag)
		}
		out[i] = v
	}
	return out, nil
}

// CompareTags returns -1, 0 or 1 as a is below, equal to or above b, comparing
// the three numbers numerically (v0.10.0 is above v0.9.0). An unparseable
// argument returns 0 and an error wrapping ErrBadTag.
func CompareTags(a, b string) (int, error) {
	pa, err := ParseTag(a)
	if err != nil {
		return 0, err
	}
	pb, err := ParseTag(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}
