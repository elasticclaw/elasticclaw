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
	if err := callback("unknown.example.com:22", addr, trustedPub); err != nil {
		t.Fatalf("unknown host key rejected: %v", err)
	}

	callback, err = knownHostsCallback()
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("unknown.example.com:22", addr, trustedPub); err != nil {
		t.Fatalf("trusted unknown host key rejected after persisting: %v", err)
	}
}

func TestKnownHostsCallbackMissingFileTrustsUnknownHostKey(t *testing.T) {
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
	if err != nil {
		t.Fatalf("missing known_hosts file rejected host key: %v", err)
	}
	if err := callback("example.com:2222", addr, pub); err != nil {
		t.Fatalf("repeated unknown host key rejected: %v", err)
	}

	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
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

	callback, err = knownHostsCallback()
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("example.com:2222", addr, pub); err != nil {
		t.Fatalf("persisted host key rejected: %v", err)
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
