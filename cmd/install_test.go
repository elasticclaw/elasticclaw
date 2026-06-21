package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestResolveInstallVersion(t *testing.T) {
	version, fetchLatest := resolveInstallVersion("", "")
	if version != "" || !fetchLatest {
		t.Fatalf("empty inputs = (%q, %t), want empty version and fetch latest", version, fetchLatest)
	}

	version, fetchLatest = resolveInstallVersion("2026.6.21", "")
	if version != "2026.6.21" || fetchLatest {
		t.Fatalf("explicit version = (%q, %t), want explicit version without fetch", version, fetchLatest)
	}

	version, fetchLatest = resolveInstallVersion("", "https://preview.elasticclaw.ai/pr/123/latest/elasticclaw-linux-amd64")
	if version != "custom" || fetchLatest {
		t.Fatalf("custom URL = (%q, %t), want custom without fetch", version, fetchLatest)
	}
}

func TestKnownHostsCallbackVerifiesServerKey(t *testing.T) {
	trustedKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustedPub, err := gossh.NewPublicKey(trustedKey)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, err := gossh.NewPublicKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(sshDir, "known_hosts")
	line := "example.com " + strings.TrimSpace(string(gossh.MarshalAuthorizedKey(trustedPub))) + "\n"
	if err := os.WriteFile(knownHosts, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	callback, err := knownHostsCallbackForPath(knownHosts, sshHostKeyPolicy{})
	if err != nil {
		t.Fatal(err)
	}

	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 22}
	if err := callback("example.com:22", addr, trustedPub); err != nil {
		t.Fatalf("trusted host key rejected: %v", err)
	}
	if err := callback("example.com:22", addr, otherPub); err == nil {
		t.Fatal("mismatched host key accepted")
	}
	if err := callback("unknown.example.com:22", addr, trustedPub); err != nil {
		if !strings.Contains(err.Error(), "unknown SSH host key") {
			t.Fatalf("unknown host key error = %q, want unknown host key message", err)
		}
		if !strings.Contains(err.Error(), gossh.FingerprintSHA256(trustedPub)) {
			t.Fatalf("unknown host key error = %q, want fingerprint", err)
		}
		if !strings.Contains(err.Error(), "ssh-keyscan -H unknown.example.com") {
			t.Fatalf("unknown host key error = %q, want ssh-keyscan hint", err)
		}
	} else {
		t.Fatal("unknown host key accepted without opt-in")
	}
}

func TestKnownHostsCallbackMissingFileRejectsUnknownHostKeyByDefault(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), ".ssh", "known_hosts")
	callback, err := knownHostsCallbackForPath(knownHosts, sshHostKeyPolicy{})
	if err != nil {
		t.Fatal(err)
	}

	key, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := gossh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}

	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 2222}
	err = callback("example.com:2222", addr, pub)
	if err == nil {
		t.Fatal("missing known_hosts accepted unknown host key without opt-in")
	}
	if !strings.Contains(err.Error(), "unknown SSH host key") {
		t.Fatalf("error = %q, want unknown host key message", err)
	}
	if !strings.Contains(err.Error(), gossh.FingerprintSHA256(pub)) {
		t.Fatalf("error = %q, want fingerprint", err)
	}
	if !strings.Contains(err.Error(), "ssh-keyscan -p 2222 -H example.com") {
		t.Fatalf("error = %q, want ssh-keyscan hint", err)
	}
	if _, statErr := os.Stat(knownHosts); !os.IsNotExist(statErr) {
		t.Fatalf("known_hosts was created in strict mode: %v", statErr)
	}
}

func TestKnownHostsCallbackTrustNewHostKeyOptInPersistsUnknownHostKey(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), ".ssh", "known_hosts")
	callback, err := knownHostsCallbackForPath(knownHosts, sshHostKeyPolicy{TrustNewHostKey: true})
	if err != nil {
		t.Fatal(err)
	}

	key, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := gossh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}

	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 2222}
	if err := callback("example.com:2222", addr, pub); err != nil {
		t.Fatalf("opt-in unknown host key rejected: %v", err)
	}
	if err := callback("example.com:2222", addr, pub); err != nil {
		t.Fatalf("repeated unknown host key rejected: %v", err)
	}

	contents, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "[example.com]:2222 "+pub.Type()) {
		t.Fatalf("known_hosts missing trusted host key entry: %s", contents)
	}
	if got := strings.Count(string(contents), "[example.com]:2222 "+pub.Type()); got != 1 {
		t.Fatalf("known_hosts has %d entries for the same trusted host key, want 1: %s", got, contents)
	}
	info, err := os.Stat(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("known_hosts mode = %v, want 0600", got)
	}

	callback, err = knownHostsCallbackForPath(knownHosts, sshHostKeyPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("example.com:2222", addr, pub); err != nil {
		t.Fatalf("persisted host key rejected: %v", err)
	}
}

func TestKnownHostsCallbackTrustNewHostKeyDoesNotAcceptChangedKey(t *testing.T) {
	trustedKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustedPub, err := gossh.NewPublicKey(trustedKey)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, err := gossh.NewPublicKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}

	knownHosts := filepath.Join(t.TempDir(), ".ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(knownHosts), 0700); err != nil {
		t.Fatal(err)
	}
	line := "example.com " + strings.TrimSpace(string(gossh.MarshalAuthorizedKey(trustedPub))) + "\n"
	if err := os.WriteFile(knownHosts, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}

	callback, err := knownHostsCallbackForPath(knownHosts, sshHostKeyPolicy{TrustNewHostKey: true})
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 22}
	if err := callback("example.com:22", addr, otherPub); err == nil {
		t.Fatal("changed host key accepted with trust-new-host-key")
	}
}

func TestInstallAndHubUpgradeExposeTrustNewHostKeyFlag(t *testing.T) {
	installFlag := installCmd.Flags().Lookup("trust-new-host-key")
	if installFlag == nil {
		t.Fatal("install command is missing --trust-new-host-key")
	}
	if strings.Contains(installFlag.Usage, "before adding it") {
		t.Fatalf("install --trust-new-host-key usage = %q, should not imply a prompt-before-write flow", installFlag.Usage)
	}
	if !strings.Contains(installFlag.Usage, "after adding it") {
		t.Fatalf("install --trust-new-host-key usage = %q, want after adding it", installFlag.Usage)
	}

	hubUpgradeFlag := hubUpgradeCmd.Flags().Lookup("trust-new-host-key")
	if hubUpgradeFlag == nil {
		t.Fatal("hub upgrade command is missing --trust-new-host-key")
	}
	if strings.Contains(hubUpgradeFlag.Usage, "before adding it") {
		t.Fatalf("hub upgrade --trust-new-host-key usage = %q, should not imply a prompt-before-write flow", hubUpgradeFlag.Usage)
	}
	if !strings.Contains(hubUpgradeFlag.Usage, "after adding it") {
		t.Fatalf("hub upgrade --trust-new-host-key usage = %q, want after adding it", hubUpgradeFlag.Usage)
	}
}

func TestSSHKeyscanHint(t *testing.T) {
	path := filepath.Join("home", ".ssh", "known_hosts")
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{
			name:     "default port",
			hostname: "example.com:22",
			want:     "ssh-keyscan -H example.com >> " + path,
		},
		{
			name:     "custom port",
			hostname: "example.com:2222",
			want:     "ssh-keyscan -p 2222 -H example.com >> " + path,
		},
		{
			name:     "host without port",
			hostname: "example.com",
			want:     "ssh-keyscan -H example.com >> " + path,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshKeyscanHint(tt.hostname, path); got != tt.want {
				t.Fatalf("sshKeyscanHint() = %q, want %q", got, tt.want)
			}
		})
	}
}
