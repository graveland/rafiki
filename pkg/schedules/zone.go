// SPDX-License-Identifier: Apache-2.0

package schedules

import (
	"fmt"
	"os"
	"strings"
)

// DaemonZone resolves the zone the daemon itself runs in. It is the
// fallback for a job whose owner has no timezone of their own: "9am" for
// that owner means the machine's 9am, so the machine must be pinned to a
// named zone.
//
// Order: $TZ (with a leading ":" stripped, the classic /etc/localtime
// spelling), then the IANA name embedded in the /etc/localtime symlink
// target. Anything unresolvable is ErrNoTimezone — the daemon logs it and
// the operator sets TZ; nothing silently falls back to UTC.
func DaemonZone() (string, error) {
	return daemonZone(os.Getenv, os.Readlink)
}

func daemonZone(getenv func(string) string, readlink func(string) (string, error)) (string, error) {
	tz := strings.TrimPrefix(getenv("TZ"), ":")
	if tz != "" && ValidateTimezone(tz) == nil {
		return tz, nil
	}
	target, err := readlink("/etc/localtime")
	if err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			name := target[i+len("zoneinfo/"):]
			if ValidateTimezone(name) == nil {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("%w: set TZ to an IANA zone name for the daemon", ErrNoTimezone)
}
