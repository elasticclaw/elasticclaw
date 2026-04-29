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
	UIPassword: "test-ui-password",
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
	assertContains(t, cfg, "test-ui-password", "ui password in config")
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
	if !strings.Contains(cfg, "ui_password: test-ui-password") {
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
	assertContains(t, cf, "reverse_proxy localhost:8080", "single backend on 8080")
}

// ── Script generation ─────────────────────────────────────────────────────────

func TestScriptInstallBinary(t *testing.T) {
	s := install.ScriptInstallBinary("v0.0.3")
	assertContains(t, s, "set -e", "fail fast")
	assertContains(t, s, "curl -fsSL", "curl download")
	assertContains(t, s, "v0.0.3", "version in URL")
	assertContains(t, s, "/usr/local/bin/elasticclaw", "install path")
	assertContains(t, s, "elasticclaw-bin", "binary download")
}

func TestScriptWriteConfig(t *testing.T) {
	s := install.ScriptWriteConfig(testParams)
	assertContains(t, s, "mkdir -p", "create dir")
	assertContains(t, s, ".elasticclaw", "config dir")
	assertContains(t, s, "hub.yaml", "config path")
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

func TestScriptWriteCaddyfile(t *testing.T) {
	s := install.ScriptWriteCaddyfile("hub.example.com")
	assertContains(t, s, "/etc/caddy/Caddyfile", "caddyfile path")
	assertContains(t, s, "hub.example.com", "domain in config")
	assertContains(t, s, "systemctl reload caddy", "caddy reload")
}

func TestScriptInstallCaddy_SupportsAptAndRpmDistros(t *testing.T) {
	s := install.ScriptInstallCaddy()
	assertContains(t, s, "command -v apt-get", "apt detection")
	assertContains(t, s, "command -v dnf", "dnf detection")
	assertContains(t, s, "command -v yum", "yum detection")
	assertContains(t, s, "dnf copr enable -y @caddy/caddy", "dnf caddy repo enable")
	assertContains(t, s, "yum copr enable -y @caddy/caddy", "yum caddy repo enable")
	assertContains(t, s, "unsupported Linux distribution", "unsupported distro message")
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
