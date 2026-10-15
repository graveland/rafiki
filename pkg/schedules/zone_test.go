// SPDX-License-Identifier: Apache-2.0

package schedules

import (
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"
)

// getenv returns a $TZ-answering getenv stub.
func getenv(tz string) func(string) string {
	return func(name string) string {
		if name == "TZ" {
			return tz
		}
		return ""
	}
}

// readlink returns a /etc/localtime-answering readlink stub.
func readlink(target string, err error) func(string) (string, error) {
	return func(string) (string, error) {
		return target, err
	}
}

// TestDaemonZonePrefersTZ pins the order: a valid $TZ wins even when the
// localtime symlink would resolve differently.
func TestDaemonZonePrefersTZ(t *testing.T) {
	c := assert.NewAborting(t)
	tz, err := daemonZone(getenv("Europe/Paris"), readlink("/usr/share/zoneinfo/America/Toronto", nil))
	c.NoError(err, "DaemonZone")
	c.Eq("Europe/Paris", tz, "a valid TZ wins over the symlink")
}

// TestDaemonZoneStripsColon pins the ":" prefix strip, the classic spelling
// of /etc/localtime-adjacent TZ values.
func TestDaemonZoneStripsColon(t *testing.T) {
	c := assert.NewAborting(t)
	tz, err := daemonZone(getenv(":Europe/Paris"), readlink("", errors.New("no symlink")))
	c.NoError(err, "DaemonZone")
	c.Eq("Europe/Paris", tz, "the leading colon is stripped")
}

// TestDaemonZoneFallsBackToLocaltimeSymlink pins the second resolution
// step: the IANA name embedded in the /etc/localtime symlink target, here
// the macOS-style path.
func TestDaemonZoneFallsBackToLocaltimeSymlink(t *testing.T) {
	c := assert.NewAborting(t)
	tz, err := daemonZone(getenv(""), readlink("/var/db/timezone/zoneinfo/America/Toronto", nil))
	c.NoError(err, "DaemonZone")
	c.Eq("America/Toronto", tz, "the zoneinfo/ suffix is the zone name")
}

// TestDaemonZoneInvalidTZFallsThrough pins that an unparseable $TZ is not a
// hard error: resolution continues to the symlink.
func TestDaemonZoneInvalidTZFallsThrough(t *testing.T) {
	c := assert.NewAborting(t)
	tz, err := daemonZone(getenv("Mars/Phobos"), readlink("/usr/share/zoneinfo/Europe/Paris", nil))
	c.NoError(err, "DaemonZone")
	c.Eq("Europe/Paris", tz, "an invalid TZ falls through to the symlink")
}

// TestDaemonZoneUnresolvableIsErrNoTimezone pins the refusal: when neither
// $TZ nor the symlink yields a valid IANA name, the caller gets ErrNoTimezone
// and the operator must set TZ.
func TestDaemonZoneUnresolvableIsErrNoTimezone(t *testing.T) {
	c := assert.NewAborting(t)

	_, err := daemonZone(getenv(""), readlink("", errors.New("no symlink")))
	c.ErrorIs(err, ErrNoTimezone, "no TZ, no symlink")

	_, err = daemonZone(getenv(""), readlink("/usr/bin/true", nil))
	c.ErrorIs(err, ErrNoTimezone, "a symlink target without zoneinfo/")

	_, err = daemonZone(getenv("Mars/Phobos"), readlink("/usr/share/zoneinfo/Mars/Phobos", nil))
	c.ErrorIs(err, ErrNoTimezone, "an invalid TZ and an invalid symlink target")

	_, err = daemonZone(getenv(":America/Nowhere"), readlink("", errors.New("no symlink")))
	c.ErrorIs(err, ErrNoTimezone, "an invalid colon-prefixed TZ")
}

// TestDaemonZoneColonOnlyTZIsNotAZone pins the edge between the two steps:
// ":" alone trims to an empty TZ, which must fall through, not match "".
func TestDaemonZoneColonOnlyTZIsNotAZone(t *testing.T) {
	c := assert.NewAborting(t)
	tz, err := daemonZone(getenv(":"), readlink("/usr/share/zoneinfo/Asia/Tokyo", nil))
	c.NoError(err, "DaemonZone")
	c.Eq("Asia/Tokyo", tz, "a colon-only TZ falls through to the symlink")
}
