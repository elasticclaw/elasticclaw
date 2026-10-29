package types

import (
	"math"
	"reflect"
	"testing"
)

func TestObservabilityDefaults(t *testing.T) {
	cfg := &ObservabilityConfig{}
	defaults := cfg.WithDefaults()
	if defaults.Enabled || defaults.OTLPEndpoint != "http://localhost:4318" || defaults.ServiceName != "elasticclaw-hub" || defaults.MetricsInterval != "15s" || *defaults.TracesSampleRatio != 1 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	if !reflect.DeepEqual(*cfg, ObservabilityConfig{}) {
		t.Fatal("resolving defaults mutated the config")
	}
	zero := 0.0
	cfg.TracesSampleRatio = &zero
	if *cfg.WithDefaults().TracesSampleRatio != 0 {
		t.Fatal("explicit zero sampling ratio was overwritten")
	}
	if err := (&HubConfig{}).Validate(); err != nil {
		t.Fatal(err)
	}
	disabled := &HubConfig{Observability: &ObservabilityConfig{MetricsInterval: "bad"}}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled block must not be validated: %v", err)
	}
}

func TestObservabilityValidationDisablesInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ObservabilityConfig)
	}{
		{"relative URL", func(c *ObservabilityConfig) { c.OTLPEndpoint = "localhost:4318" }},
		{"invalid scheme", func(c *ObservabilityConfig) { c.OTLPEndpoint = "ftp://localhost" }},
		{"missing host", func(c *ObservabilityConfig) { c.OTLPEndpoint = "http:///path" }},
		{"invalid port", func(c *ObservabilityConfig) { c.OTLPEndpoint = "http://localhost:bad" }},
		{"negative ratio", func(c *ObservabilityConfig) { v := -0.1; c.TracesSampleRatio = &v }},
		{"large ratio", func(c *ObservabilityConfig) { v := 1.1; c.TracesSampleRatio = &v }},
		{"NaN ratio", func(c *ObservabilityConfig) { v := math.NaN(); c.TracesSampleRatio = &v }},
		{"invalid duration", func(c *ObservabilityConfig) { c.MetricsInterval = "later" }},
		{"zero duration", func(c *ObservabilityConfig) { c.MetricsInterval = "0s" }},
		{"negative duration", func(c *ObservabilityConfig) { c.MetricsInterval = "-1s" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &HubConfig{Observability: &ObservabilityConfig{Enabled: true}}
			tc.mutate(cfg.Observability)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation warning")
			}
			if cfg.Observability.Enabled {
				t.Fatal("invalid observability remains enabled")
			}
		})
	}
}
