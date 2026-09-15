package hub

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestDaytonaModelConfigKeepsShellMetacharactersLiteral(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(home, "unexpected-substitution")
	payload := `$(printf injected > ` + shellQuote(marker) + `)` + "`printf second`'\"\\\n"
	mainModel, childModel := "anthropic/"+payload, "openai/"+payload
	password := "gateway-" + payload
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "main", Provider: "anthropic", APIKey: "fixture"}, {Name: "child", Provider: "openai", APIKey: "fixture"}}}
	plan, err := buildAgentBootstrapPlan(cfg, "main", mainModel, &types.SubagentConfig{LLMKey: "child", Model: childModel})
	if err != nil {
		t.Fatal(err)
	}
	// Redirect only the config output directory; exercise the actual Daytona
	// export builder and the real provider script with the user-supplied models.
	patch := daytonaOpenClawConfigPatch(mainModel, password, "", "export HOME="+shellQuote(home)+"; "+plan.ProviderConfig)
	cmd := exec.Command("bash", "-c", patch)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("patch failed: %v: %s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("model command substitution executed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".openclaw/openclaw.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Agents struct {
			Defaults struct {
				Model     string `json:"model"`
				Subagents struct {
					Model string `json:"model"`
				} `json:"subagents"`
			} `json:"defaults"`
		} `json:"agents"`
		Gateway struct {
			Auth struct {
				Password string `json:"password"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.Agents.Defaults.Model != mainModel || config.Agents.Defaults.Subagents.Model != childModel || config.Gateway.Auth.Password != password {
		t.Fatalf("literal model/password data changed: %s", raw)
	}
}

func TestStoredModelOnboardAndCLIInputsRemainData(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(home, "unexpected-substitution")
	modelID := `worker$(printf injected > ` + shellQuote(marker) + `)` + "`printf second`'\"\\"
	flags := buildOnboardFlags([]*types.LLMKeyConfig{{Name: "local", Provider: "ollama"}}, "local", "ollama/"+modelID)
	cmd := exec.Command("bash", "-c", `capture() { printf '%s\0' "$@"; }; capture `+flags)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("onboard flags failed: %v: %s", err, output)
	}
	args := strings.Split(string(output), "\x00")
	if len(args) < 3 || args[len(args)-2] != modelID {
		t.Fatalf("model argument changed: %q", args)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("onboard model substitution executed: %v", err)
	}
	for _, provider := range []string{"codex/", "grok/"} {
		if daytonaInstallCodingModelCLICommand(provider+modelID) != daytonaInstallCodingModelCLICommand(provider+"normal") {
			t.Fatalf("CLI installation interpolates model tail for %s", provider)
		}
	}
}
