package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestVerifySSHHostKeyTrustsFirstKeyAndRejectsChange(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	s := &Server{db: db}

	key1 := testSSHPublicKey(t)
	key2 := testSSHPublicKey(t)
	host := "127.0.0.1:2222"

	if err := s.verifySSHHostKey(host, key1); err != nil {
		t.Fatalf("first key should be trusted: %v", err)
	}
	if err := s.verifySSHHostKey(host, key1); err != nil {
		t.Fatalf("same key should be accepted: %v", err)
	}
	if err := s.verifySSHHostKey(host, key2); err == nil {
		t.Fatal("changed key should be rejected")
	}
}

func testSSHPublicKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	return key
}
