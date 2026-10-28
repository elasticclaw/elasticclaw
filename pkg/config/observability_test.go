package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestObservabilityConfigRoundTrip(t *testing.T) {
	for _, merge := range []bool{false, true} {
		t.Run(map[bool]string{false: "without secret merge", true: "with secret merge"}[merge], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.yaml")
			t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
			if err := os.WriteFile(path, []byte("secrets:\n  existing: retained\nobservability:\n  enabled: true\n  otlp_endpoint: https://collector.example/otlp\n  service_name: custom-hub\n  environment: faster\n  traces_sample_ratio: 0\n  metrics_interval: 30s\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadHubConfig()
			if err != nil {
				t.Fatal(err)
			}
			original := *cfg.Observability
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			cfg.Secrets = nil
			if err := saveHubConfig(cfg, merge); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadHubConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Observability, &original) {
				t.Fatalf("observability changed: got %+v, want %+v", loaded.Observability, original)
			}
			if merge && loaded.Secrets["existing"] != "retained" {
				t.Fatal("disk secret lost")
			}
			if cfg.Secrets != nil {
				t.Fatal("save mutated caller secrets")
			}
		})
	}
}

func TestObservabilityConfigOmittedRoundTrip(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	cfg := &types.HubConfig{}
	if err := SaveHubConfigNoSecretMerge(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Observability != nil {
		t.Fatal("save added observability block")
	}
}
