package types

import (
	"fmt"
	"math"
	"net/url"
	"time"
)

// WithDefaults resolves omitted settings without changing the persisted config.
func (c *ObservabilityConfig) WithDefaults() ObservabilityConfig {
	var cfg ObservabilityConfig
	if c != nil {
		cfg = *c
	}
	if cfg.OTLPEndpoint == "" {
		cfg.OTLPEndpoint = "http://localhost:4318"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "elasticclaw-hub"
	}
	if cfg.TracesSampleRatio == nil {
		ratio := 1.0
		cfg.TracesSampleRatio = &ratio
	}
	if cfg.MetricsInterval == "" {
		cfg.MetricsInterval = "15s"
	}
	return cfg
}

// Validate ignores disabled blocks so an unused setting never surfaces warnings.
func (c *ObservabilityConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	cfg := c.WithDefaults()
	endpoint, err := url.Parse(cfg.OTLPEndpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("otlp_endpoint must be an absolute HTTP or HTTPS base URL without credentials, query, or fragment")
	}
	ratio := *cfg.TracesSampleRatio
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
		return fmt.Errorf("traces_sample_ratio must be between 0 and 1")
	}
	interval, err := time.ParseDuration(cfg.MetricsInterval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("metrics_interval must be a positive duration")
	}
	return nil
}
