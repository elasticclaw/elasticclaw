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
	}
}
