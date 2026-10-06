// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnvDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	cfg, err := ConfigFromEnv(envFrom(nil))
	c.NoError(err, "ConfigFromEnv(empty)")
	c.Eq("", cfg.Image, "Image default")
	c.Eq(protocol.NetworkEgress, cfg.Network, "Network default")
	c.Eq(168*time.Hour, cfg.TTL, "TTL default")
	c.Eq(720*time.Hour, cfg.MaxTTL, "MaxTTL default")
	c.Eq(8, cfg.MaxPerOwner, "MaxPerOwner default")
	c.Eq(60*time.Second, cfg.SweepInterval, "SweepInterval default")
	c.Eq(int64(0), cfg.ChildMaxMemoryBytes, "ChildMaxMemoryBytes default")
	c.Eq(0.0, cfg.ChildMaxCPUs, "ChildMaxCPUs default")
	c.Eq(int64(0), cfg.ChildMaxPids, "ChildMaxPids default")
}

func TestConfigFromEnvOverrides(t *testing.T) {
	c := assert.NewAborting(t)
	cfg, err := ConfigFromEnv(envFrom(map[string]string{
		EnvImage:          "img:2",
		EnvNetwork:        "none",
		EnvTTL:            "1h",
		EnvMaxTTL:         "2h",
		EnvMaxPerOwner:    "3",
		EnvSweepInterval:  "5s",
		EnvChildMaxMemory: "1073741824",
		EnvChildMaxCPUs:   "1.5",
		EnvChildMaxPids:   "64",
	}))
	c.NoError(err, "ConfigFromEnv(overrides)")
	c.Eq("img:2", cfg.Image, "Image")
	c.Eq(protocol.NetworkNone, cfg.Network, "Network")
	c.Eq(time.Hour, cfg.TTL, "TTL")
	c.Eq(2*time.Hour, cfg.MaxTTL, "MaxTTL")
	c.Eq(3, cfg.MaxPerOwner, "MaxPerOwner")
	c.Eq(5*time.Second, cfg.SweepInterval, "SweepInterval")
	c.Eq(int64(1073741824), cfg.ChildMaxMemoryBytes, "ChildMaxMemoryBytes")
	c.Eq(1.5, cfg.ChildMaxCPUs, "ChildMaxCPUs")
	c.Eq(int64(64), cfg.ChildMaxPids, "ChildMaxPids")
}

func TestConfigFromEnvRejectsMalformed(t *testing.T) {
	tests := []struct {
		subtest string
		env     map[string]string
		wantVar string
	}{
		{"bad duration", map[string]string{EnvTTL: "soon"}, EnvTTL},
		{"negative duration", map[string]string{EnvTTL: "-1h"}, EnvTTL},
		{"bad int", map[string]string{EnvMaxPerOwner: "lots"}, EnvMaxPerOwner},
		{"negative int", map[string]string{EnvChildMaxPids: "-1"}, EnvChildMaxPids},
		{"bad float", map[string]string{EnvChildMaxCPUs: "many"}, EnvChildMaxCPUs},
		{"negative float", map[string]string{EnvChildMaxCPUs: "-0.5"}, EnvChildMaxCPUs},
		{"bad network", map[string]string{EnvNetwork: "bridge"}, EnvNetwork},
		{"ttl_zero", map[string]string{EnvTTL: "0s"}, EnvTTL},
		{"max_ttl_zero", map[string]string{EnvMaxTTL: "0s"}, EnvMaxTTL},
		{"cpus nan", map[string]string{EnvChildMaxCPUs: "NaN"}, EnvChildMaxCPUs},
		{"cpus inf", map[string]string{EnvChildMaxCPUs: "Inf"}, EnvChildMaxCPUs},
		{"cpus neg inf", map[string]string{EnvChildMaxCPUs: "-Inf"}, EnvChildMaxCPUs},
		{"ttl over max", map[string]string{EnvTTL: "1000h"}, EnvMaxTTL},
		{"zero sweep interval", map[string]string{EnvSweepInterval: "0s"}, EnvSweepInterval},
		{"negative sweep interval", map[string]string{EnvSweepInterval: "-5s"}, EnvSweepInterval},
	}
	for _, tt := range tests {
		t.Run(tt.subtest, func(t *testing.T) {
			c := assert.NewAborting(t)
			_, err := ConfigFromEnv(envFrom(tt.env))
			c.Error(err, "ConfigFromEnv(%v) = nil error, want error naming %s", tt.env, tt.wantVar)
			c.StrContains(err.Error(), tt.wantVar, "ConfigFromEnv(%v) = %v, want it to name", tt.env, err)
		})
	}
}
