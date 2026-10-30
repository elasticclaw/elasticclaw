package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUpdateHubObservabilityPreservesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	original := "public_url: https://hub.example.com\ntoken: keep-secret\nsecrets:\n  KEEP: preserved\nfuture_extension:\n  nested: [one, two]\nobservability:\n  enabled: false\n  service_name: custom\n  environment: staging\n  traces_sample_ratio: 0\n  metrics_interval: 30s\n  future: preserved\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	environment := "production"
	for _, enabled := range []bool{true, false, true} {
		if err := UpdateHubObservability(enabled, &environment); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadHubConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Token != "keep-secret" || cfg.PublicURL != "https://hub.example.com" || cfg.Secrets["KEEP"] != "preserved" {
			t.Fatal("unrelated fields changed")
		}
		obs := cfg.Observability
		if obs.Enabled != enabled || obs.ServiceName != "custom" || obs.Environment != "production" || *obs.TracesSampleRatio != 0 || obs.MetricsInterval != "30s" {
			t.Fatalf("observability fields changed: %+v", obs)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if raw["future_extension"] == nil || raw["observability"].(map[string]any)["future"] != "preserved" {
			t.Fatal("unknown fields lost")
		}
	}
}

func TestUpdateHubObservabilityPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	if err := os.WriteFile(path, []byte("token: keep\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHubObservability(true, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode %v, want 0640", info.Mode().Perm())
	}
}

func TestUpdateHubObservabilityWritesTheLoadedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", missing)
	if _, err := os.Stat("/etc/elasticclaw/hub.yaml"); err == nil {
		t.Skip("system hub.yaml present")
	}
	if err := UpdateHubObservability(true, nil); !errors.Is(err, ErrNoHubConfig) {
		t.Fatalf("err = %v, want ErrNoHubConfig", err)
	}
	homeConfig := filepath.Join(home, ".elasticclaw", "hub.yaml")
	if err := os.MkdirAll(filepath.Dir(homeConfig), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeConfig, []byte("token: keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHubObservability(true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created a config at the missing $ELASTICCLAW_HUB_CONFIG path, shadowing the real one")
	}
	data, err := os.ReadFile(homeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "token: keep") || !strings.Contains(string(data), "enabled: true") {
		t.Fatalf("loaded config not updated in place:\n%s", data)
	}
}

func TestUpdateHubObservabilityDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	if err := os.WriteFile(path, []byte("token: keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHubObservability(true, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Observability.Enabled || *cfg.Observability.TracesSampleRatio != 0.25 || cfg.Observability.OTLPEndpoint != "http://localhost:4318" {
		t.Fatalf("defaults: %+v", cfg.Observability)
	}
}

func TestUpdateHubObservabilityClearsEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	if err := os.WriteFile(path, []byte("observability:\n  enabled: true\n  environment: production\n"), 0600); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := UpdateHubObservability(true, &empty); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Observability.Environment != "" {
		t.Fatal("environment not cleared")
	}
}
