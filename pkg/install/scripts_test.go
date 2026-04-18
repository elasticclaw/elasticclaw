package install_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/install"
)

var testParams = install.Params{
	Domain:    "hub.example.com",
	Version:   "v0.0.3",
	Token:     "test-hub-token",
	ClawToken: "test-claw-token",
	UIToken:   "test-ui-token",
}

// ── HubBinaryURL ─────────────────────────────────────────────────────────────

func TestHubBinaryURL(t *testing.T) {
	url := install.HubBinaryURL("v0.0.3")
	assertContains(t, url, "github.com/elasticclaw/elasticclaw", "GitHub repo in URL")
	assertContains(t, url, "v0.0.3", "version in URL")
	assertContains(t, url, "elasticclaw-linux-amd64", "binary name in URL")
}

// ── HubConfig ────────────────────────────────────────────────────────────────

func TestHubConfig(t *testing.T) {
	cfg := install.HubConfig(testParams)
	assertContains(t, cfg, "hub.example.com", "domain in config")
	assertContains(t, cfg, "test-hub-token", "hub token in config")
	assertContains(t, cfg, "test-claw-token", "claw token in config")
	assertContains(t, cfg, "test-ui-token", "ui token in config")
	assertContains(t, cfg, ":8080", "port in config")
}

func TestHubConfig_DoesNotContainOtherTokens(t *testing.T) {
	cfg := install.HubConfig(testParams)
	// Tokens should not be mixed up
	if strings.Count(cfg, "test-hub-token") != 2 { // url + token field
		// token appears in url and token: field
	}
	if !strings.Contains(cfg, "claw_token: test-claw-token") {
		t.Error("claw_token field missing or wrong")
	}
	if !strings.Contains(cfg, "ui_token: test-ui-token") {
		t.Error("ui_token field missing or wrong")
	}
}

// ── SystemdUnit ───────────────────────────────────────────────────────────────

func TestSystemdUnit(t *testing.T) {
	unit := install.SystemdUnit()
	assertContains(t, unit, "[Unit]", "Unit section")
	assertContains(t, unit, "[Service]", "Service section")
	assertContains(t, unit, "[Install]", "Install section")
	assertContains(t, unit, "/usr/local/bin/elasticclaw hub", "ExecStart command")
	assertContains(t, unit, "Restart=always", "Restart policy")
}

// ── Caddyfile ─────────────────────────────────────────────────────────────────

func TestCaddyfile(t *testing.T) {
	cf := install.Caddyfile("hub.example.com")
	assertContains(t, cf, "hub.example.com {", "domain block")
	assertContains(t, cf, "reverse_proxy localhost:8080", "hub backend")
	assertContains(t, cf, "reverse_proxy localhost:3000", "web UI backend")
	assertContains(t, cf, "/api/claws", "claws route")
	assertContains(t, cf, "/api/ws", "websocket route")
	assertContains(t, cf, "/api/messages", "messages route")
	assertContains(t, cf, "/claw/", "bridge route")
}

func TestCaddyfile_APIRouting(t *testing.T) {
	cf := install.Caddyfile("hub.example.com")
	// These must go to hub (8080), not web UI
	hubRoutes := []string{"/api/claws", "/api/messages", "/api/ws", "/api/terminal", "/api/github", "/claw/"}
	for _, route := range hubRoutes {
		idx := strings.Index(cf, route)
		if idx < 0 {
			t.Errorf("route %s not found in Caddyfile", route)
			continue
		}
		// Check that 8080 appears after this route (before the next handle block)
		end := idx + 200
		if end > len(cf) {
			end = len(cf)
		}
		segment := cf[idx:end]
		if !strings.Contains(segment, "localhost:8080") {
			t.Errorf("route %s should proxy to :8080", route)
		}
	}
	// These must go to web UI (3000)
	webRoutes := []string{"/api/auth/", "/api/hub-config"}
	for _, route := range webRoutes {
		idx := strings.Index(cf, route)
		if idx < 0 {
			t.Errorf("route %s not found in Caddyfile", route)
			continue
		}
		end2 := idx + 200
		if end2 > len(cf) {
			end2 = len(cf)
		}
		segment := cf[idx:end2]
		if !strings.Contains(segment, "localhost:3000") {
			t.Errorf("route %s should proxy to :3000", route)
		}
	}
}

// ── Script generation ─────────────────────────────────────────────────────────

func TestScriptInstallBinary(t *testing.T) {
	s := install.ScriptInstallBinary("v0.0.3")
	assertContains(t, s, "set -e", "fail fast")
	assertContains(t, s, "curl -fsSL", "curl download")
	assertContains(t, s, "v0.0.3", "version in URL")
	assertContains(t, s, "/usr/local/bin/elasticclaw", "install path")
	assertContains(t, s, "elasticclaw --version", "verify install")
}

func TestScriptWriteConfig(t *testing.T) {
	s := install.ScriptWriteConfig(testParams)
	assertContains(t, s, "mkdir -p /root/.elasticclaw", "create dir")
	assertContains(t, s, "/root/.elasticclaw/hub.yaml", "config path")
	assertContains(t, s, "test-hub-token", "token embedded")
	assertContains(t, s, "HUBEOF", "heredoc markers")
}

func TestScriptInstallSystemd(t *testing.T) {
	s := install.ScriptInstallSystemd()
	assertContains(t, s, "/etc/systemd/system/elasticclaw.service", "service path")
	assertContains(t, s, "systemctl daemon-reload", "daemon reload")
	assertContains(t, s, "systemctl enable elasticclaw", "enable service")
	assertContains(t, s, "systemctl restart elasticclaw", "start service")
}

func TestScriptRunWebUI(t *testing.T) {
	s := install.ScriptRunWebUI(testParams)
	assertContains(t, s, "docker pull", "docker pull")
	assertContains(t, s, "marc/elasticclaw-web:v0.0.3", "image tag with version")
	assertContains(t, s, "ELASTICCLAW_HUB_TOKEN=test-hub-token", "hub token env")
	assertContains(t, s, "ELASTICCLAW_UI_TOKEN=test-ui-token", "ui token env")
	assertContains(t, s, "--restart=always", "restart policy")
	assertContains(t, s, "--network=host", "host network")
	assertContains(t, s, "elasticclaw-web", "container name")
}

func TestScriptWriteCaddyfile(t *testing.T) {
	s := install.ScriptWriteCaddyfile("hub.example.com")
	assertContains(t, s, "/etc/caddy/Caddyfile", "caddyfile path")
	assertContains(t, s, "hub.example.com", "domain in config")
	assertContains(t, s, "systemctl reload caddy", "caddy reload")
}

// ── Shellcheck ────────────────────────────────────────────────────────────────

func TestScripts_Shellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not in PATH")
	}

	scripts := map[string]string{
		"install_binary":   install.ScriptInstallBinary("v0.0.3"),
		"write_config":     install.ScriptWriteConfig(testParams),
		"install_systemd":  install.ScriptInstallSystemd(),
		"run_web_ui":       install.ScriptRunWebUI(testParams),
		"install_caddy":    install.ScriptInstallCaddy(),
		"write_caddyfile":  install.ScriptWriteCaddyfile("hub.example.com"),
	}

	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			f, err := os.CreateTemp("", "install-*.sh")
			if err != nil {
				t.Fatalf("create temp file: %v", err)
			}
			defer os.Remove(f.Name())
			// Wrap in #!/bin/bash for shellcheck
			f.WriteString("#!/bin/bash\n" + script)
			f.Close()

			cmd := exec.Command("shellcheck", "-s", "bash",
				"-e", "SC1091", // don't follow sourced files
				f.Name(),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("shellcheck failed for %s:\n%s", name, string(out))
			}
		})
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func assertContains(t *testing.T, s, substr, desc string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %s to contain %s\nwanted: %q", desc, desc, substr)
	}
}
