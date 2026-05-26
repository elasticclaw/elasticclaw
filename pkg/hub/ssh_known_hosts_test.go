package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"sync"
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

func TestVerifySSHHostKeyConcurrentFirstUse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	s := &Server{db: db}

	key := testSSHPublicKey(t)
	host := "127.0.0.1:2223"

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.verifySSHHostKey(host, key)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent first-use key should be accepted: %v", err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ssh_known_hosts WHERE host=?`, host).Scan(&count); err != nil {
		t.Fatalf("count known hosts: %v", err)
	}
	if count != 1 {
		t.Fatalf("known host rows = %d, want 1", count)
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
