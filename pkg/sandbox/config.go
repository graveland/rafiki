// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// Env vars the daemon reads for sandbox defaults. They live here, with the
// defaults, so changing one is a single edit.
const (
	EnvImage          = "RAFIKI_SANDBOX_IMAGE"
	EnvNetwork        = "RAFIKI_SANDBOX_NETWORK"
	EnvTTL            = "RAFIKI_SANDBOX_TTL"
	EnvMaxTTL         = "RAFIKI_SANDBOX_MAX_TTL"
	EnvMaxPerOwner    = "RAFIKI_SANDBOX_MAX_PER_OWNER"
	EnvSweepInterval  = "RAFIKI_SANDBOX_SWEEP_INTERVAL"
	EnvChildMaxMemory = "RAFIKI_SANDBOX_CHILD_MAX_MEMORY_BYTES"
	EnvChildMaxCPUs   = "RAFIKI_SANDBOX_CHILD_MAX_CPUS"
	EnvChildMaxPids   = "RAFIKI_SANDBOX_CHILD_MAX_PIDS"
)

// Defaults, all in one place. The child_max_* caps default to 0 = no clamp.
const (
	DefaultNetwork       = protocol.NetworkEgress
	DefaultTTL           = 168 * time.Hour
	DefaultMaxTTL        = 720 * time.Hour
	DefaultMaxPerOwner   = 8
	DefaultSweepInterval = 60 * time.Second
)

// Config is the daemon's sandbox configuration, resolved from the environment.
type Config struct {
	Image               string
	Network             protocol.NetworkMode
	TTL, MaxTTL         time.Duration
	MaxPerOwner         int
	SweepInterval       time.Duration
	ChildMaxMemoryBytes int64
	ChildMaxCPUs        float64
	ChildMaxPids        int64
}

// ConfigFromEnv resolves the sandbox configuration. An unset (empty) variable
// takes its default; a malformed value, a non-positive duration, a negative
// number, or TTL > MaxTTL is an error naming the variable.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Image:         getenv(EnvImage),
		Network:       DefaultNetwork,
		TTL:           DefaultTTL,
		MaxTTL:        DefaultMaxTTL,
		MaxPerOwner:   DefaultMaxPerOwner,
		SweepInterval: DefaultSweepInterval,
	}

	if v := getenv(EnvNetwork); v != "" {
		switch protocol.NetworkMode(v) {
		case protocol.NetworkEgress, protocol.NetworkNone:
			cfg.Network = protocol.NetworkMode(v)
		default:
			return Config{}, fmt.Errorf("%s: %q is not one of %q or %q",
				EnvNetwork, v, protocol.NetworkEgress, protocol.NetworkNone)
		}
	}

	var err error
	if cfg.TTL, err = durationEnv(getenv, EnvTTL, DefaultTTL); err != nil {
		return Config{}, err
	}
	if cfg.MaxTTL, err = durationEnv(getenv, EnvMaxTTL, DefaultMaxTTL); err != nil {
		return Config{}, err
	}
	if cfg.TTL > cfg.MaxTTL {
		return Config{}, fmt.Errorf("%s (%s) exceeds %s (%s)", EnvTTL, cfg.TTL, EnvMaxTTL, cfg.MaxTTL)
	}
	if cfg.SweepInterval, err = durationEnv(getenv, EnvSweepInterval, DefaultSweepInterval); err != nil {
		return Config{}, err
	}
	if cfg.MaxPerOwner, err = intEnv(getenv, EnvMaxPerOwner, DefaultMaxPerOwner); err != nil {
		return Config{}, err
	}
	if cfg.ChildMaxMemoryBytes, err = int64Env(getenv, EnvChildMaxMemory, 0); err != nil {
		return Config{}, err
	}
	if cfg.ChildMaxCPUs, err = floatEnv(getenv, EnvChildMaxCPUs, 0); err != nil {
		return Config{}, err
	}
	if cfg.ChildMaxPids, err = int64Env(getenv, EnvChildMaxPids, 0); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// durationEnv parses a duration-valued variable. An unset variable takes def;
// a malformed or non-positive value is refused, naming the variable. Every
// duration in this config must be strictly positive: a zero TTL would either
// expire a named sandbox instantly or silently mean "never", both violating
// the rule that a named sandbox always expires, and a zero sweep interval
// would spin.
func durationEnv(getenv func(string) string, name string, def time.Duration) (time.Duration, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid duration: %w", name, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: %s must be positive", name, d)
	}
	return d, nil
}

func intEnv(getenv func(string) string, name string, def int) (int, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid integer: %w", name, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: %d must not be negative", name, n)
	}
	return n, nil
}

func int64Env(getenv func(string) string, name string, def int64) (int64, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid integer: %w", name, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: %d must not be negative", name, n)
	}
	return n, nil
}

func floatEnv(getenv func(string) string, name string, def float64) (float64, error) {
	v := getenv(name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid number: %w", name, v, err)
	}
	// ParseFloat accepts "NaN" and "Inf"; neither is < 0, so without this a
	// NaN cap would silently disable the child clamp (see validateLimits).
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s: %q is not a finite number", name, v)
	}
	if f < 0 {
		return 0, fmt.Errorf("%s: %g must not be negative", name, f)
	}
	return f, nil
}
