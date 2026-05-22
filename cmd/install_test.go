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

	callback, err := knownHostsCallback()
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
	if err := callback("unknown.example.com:22", addr, trustedPub); err == nil {
		t.Fatal("unknown host key accepted")
	} else if !strings.Contains(err.Error(), "ssh-keyscan -H unknown.example.com") {
		t.Fatalf("unknown host key error missing ssh-keyscan hint: %v", err)
	}
}

func TestKnownHostsCallbackMissingFileRejectsWithHint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	callback, err := knownHostsCallback()
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
		t.Fatal("missing known_hosts file accepted host key")
	}
	if !strings.Contains(err.Error(), "known_hosts file not found") {
		t.Fatalf("missing known_hosts error missing context: %v", err)
	}
	if !strings.Contains(err.Error(), "ssh-keyscan -p 2222 -H example.com") {
		t.Fatalf("missing known_hosts error missing port-aware ssh-keyscan hint: %v", err)
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
