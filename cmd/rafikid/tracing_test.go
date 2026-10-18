// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/telemetry"
)

func TestSetupTracingResolve(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		cfg      telemetry.Config
		enabled  bool
		protocol string
		endpoint string
		err      string // empty means nil
	}{
		{
			name:     "nothing set is disabled",
			protocol: "http/protobuf",
		},
		{
			name:     "cfg endpoint enables http/protobuf",
			cfg:      telemetry.Config{Endpoint: "http://collector:4318"},
			enabled:  true,
			protocol: "http/protobuf",
			endpoint: "http://collector:4318",
		},
		{
			name: "env endpoint beats cfg endpoint",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://env:4318"},
			cfg:  telemetry.Config{Endpoint: "http://collector:4318"},
			// The endpoint stays empty so the exporter reads the environment.
			enabled:  true,
			protocol: "http/protobuf",
		},
		{
			name:     "env protocol beats cfg protocol",
			env:      map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			cfg:      telemetry.Config{Endpoint: "http://collector:4317", Protocol: "http/protobuf"},
			enabled:  true,
			protocol: "grpc",
			endpoint: "http://collector:4317",
		},
		{
			name: "traces protocol beats protocol",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/protobuf",
				"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc",
			},
			cfg:      telemetry.Config{Endpoint: "http://collector:4317"},
			enabled:  true,
			protocol: "grpc",
			endpoint: "http://collector:4317",
		},
		{
			name: "bad protocol errors",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://env:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "zipkin"},
			err:  `otlp protocol "zipkin"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			enabled, protocol, endpoint, err := resolveOTLP(getenv, tc.cfg)
			if tc.err != "" {
				if err == nil {
					t.Fatalf("resolveOTLP succeeded, want error containing %q", tc.err)
				}
				if !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %q does not contain %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveOTLP: %v", err)
			}
			c := assert.NewCollecting(t)
			c.Eq(tc.enabled, enabled, "enabled")
			c.Eq(tc.protocol, protocol, "protocol")
			c.Eq(tc.endpoint, endpoint, "endpoint")
		})
	}
}

// TestSetupTracingEnabledBuildsProvider pins that an endpoint actually yields a
// working provider. The resource merge used by setupTracing is schema-version
// sensitive: resource.Default() carries the SDK's semantic-convention URL, and
// merging attributes tagged with a different version fails with "conflicting
// Schema URL", which the daemon swallows as "tracing setup failed". A unit test
// that only exercised resolveOTLP could not see that.
func TestSetupTracingEnabledBuildsProvider(t *testing.T) {
	c := assert.NewCollecting(t)
	// The exporter is constructed, not dialled, so an unused local port is
	// enough to prove setup completes.
	tp, shutdown, err := setupTracing(context.Background(), telemetry.Config{Endpoint: "http://127.0.0.1:1"}, "test-daemon")
	c.NoError(err, "setupTracing with an endpoint")
	c.NotNil(tp, "provider")
	defer shutdown()
}
