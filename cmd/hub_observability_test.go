package cmd

import (
	"io"
	"os"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func TestHubInitObservability(t *testing.T) {
	previousEnabled, previousPrint := initObservability, initPrintOnly
	previousToken, previousClaw, previousURL := initToken, initClawToken, initPublicURL
	defer func() {
		initObservability, initPrintOnly = previousEnabled, previousPrint
		initToken, initClawToken, initPublicURL = previousToken, previousClaw, previousURL
	}()
	initPrintOnly, initToken, initClawToken, initPublicURL = true, "user-token", "claw-token", "https://hub.example.com"
	for _, enabled := range []bool{false, true} {
		initObservability = enabled
		file, err := os.CreateTemp(t.TempDir(), "init-output")
		if err != nil {
			t.Fatal(err)
		}
		originalStdout := os.Stdout
		os.Stdout = file
		err = runHubInit(hubInitCmd, nil)
		os.Stdout = originalStdout
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		var cfg types.HubConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		if enabled {
			if cfg.Observability == nil || !cfg.Observability.Enabled || cfg.Observability.OTLPEndpoint != "http://localhost:4318" || *cfg.Observability.TracesSampleRatio != 0.25 {
				t.Fatalf("observability config: %+v", cfg.Observability)
			}
		} else if cfg.Observability != nil {
			t.Fatal("observability must remain opt-in")
		}
	}
}
