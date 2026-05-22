package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGatewayClientConfig(t *testing.T, home, config string) {
	t.Helper()
	cfgDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(filepath.Join(cfgDir, "identity"), 0700); err != nil {
		t.Fatalf("create openclaw dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "openclaw.json"), []byte(config), 0600); err != nil {
		t.Fatalf("write openclaw config: %v", err)
	}
	device := `{"deviceId":"device-1","publicKeyPem":"pub","privateKeyPem":"priv"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "identity", "device.json"), []byte(device), 0600); err != nil {
		t.Fatalf("write device config: %v", err)
	}
}

func TestLoadGatewayClientUsesRemotePasswordFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_GATEWAY_PASSWORD", "")
	t.Setenv("OPENCLAW_GATEWAY_PASSWORD", "")
	writeGatewayClientConfig(t, home, `{
  "gateway": {
    "auth": {"mode": "password"},
    "remote": {"password": "remote-password"}
  }
}`)

	client, err := loadGatewayClient("localhost:18789")
	if err != nil {
		t.Fatalf("load gateway client: %v", err)
	}
	if client.password != "remote-password" {
		t.Fatalf("password = %q, want remote-password", client.password)
	}
	if client.token != "" {
		t.Fatalf("token = %q, want empty when password is configured", client.token)
	}
}

func TestLoadGatewayClientOpenClawPasswordEnvOverridesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_GATEWAY_PASSWORD", "")
	t.Setenv("OPENCLAW_GATEWAY_PASSWORD", "env-password")
	writeGatewayClientConfig(t, home, `{
  "gateway": {
    "auth": {"mode": "password", "password": "auth-password"},
    "remote": {"password": "remote-password"}
  }
}`)

	client, err := loadGatewayClient("localhost:18789")
	if err != nil {
		t.Fatalf("load gateway client: %v", err)
	}
	if client.password != "env-password" {
		t.Fatalf("password = %q, want env-password", client.password)
	}
}
