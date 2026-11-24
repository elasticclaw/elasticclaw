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

func TestReplicatedSSHEndpointHostKeyLifetime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   string
		oldVM      string
		newVM      string
		storedHost string
		wantNewKey bool
		wantError  bool
	}{
		{name: "first VM", provider: "replicated", newVM: "vm-new", wantNewKey: true},
		{name: "replacement VM", provider: "replicated", oldVM: "vm-old", newVM: "vm-new", storedHost: "127.0.0.1", wantNewKey: true},
		{name: "same VM", provider: "replicated", oldVM: "vm-old", newVM: "vm-old", storedHost: "127.0.0.1"},
		{name: "other provider", provider: "docker", oldVM: "vm-old", newVM: "vm-old", wantError: true},
		{name: "outdated bootstrap", provider: "replicated", oldVM: "vm-current", newVM: "vm-old", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := openDB(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			s := &Server{db: db}
			if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('tenant','Tenant','token','claw-token',?)`, now()); err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`INSERT INTO claws(id, tenant_id, name, provider, provider_id, status, ssh_host, ssh_port, created_at)
				VALUES('claw', 'tenant', 'claw', ?, ?, 'provisioning', ?, 2222, ?)`, tc.provider, tc.oldVM, tc.storedHost, now())
			if err != nil {
				t.Fatal(err)
			}
			oldKey, newKey := testSSHPublicKey(t), testSSHPublicKey(t)
			const addr = "127.0.0.1:2222"
			const otherAddr = "127.0.0.1:2223"
			for _, host := range []string{addr, otherAddr} {
				if err := s.verifySSHHostKey(host, oldKey); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.wantError {
				if err := s.assignReplicatedVM("claw", tc.newVM); err != nil {
					t.Fatal(err)
				}
			}
			err = s.storeReplicatedSSHEndpoint("claw", tc.newVM, "127.0.0.1", 2222, "elasticclaw")
			if (err != nil) != tc.wantError {
				t.Fatalf("store endpoint error = %v, want error %v", err, tc.wantError)
			}
			// Exercise the callback used by SSH connections without binding a port.
			err = s.sshHostKeyCallback(addr)(addr, nil, newKey)
			if (err == nil) != tc.wantNewKey {
				t.Fatalf("new key error = %v, want trusted %v", err, tc.wantNewKey)
			}
			if err := s.verifySSHHostKey(otherAddr, newKey); err == nil {
				t.Fatal("pin for another port was cleared")
			}
			trustedKey := oldKey
			if tc.wantNewKey {
				trustedKey = newKey
			}
			if !tc.wantError {
				// Reassigning and bootstrapping the same VM must preserve its pin.
				if err := s.assignReplicatedVM("claw", tc.newVM); err != nil {
					t.Fatal(err)
				}
				if err := s.storeReplicatedSSHEndpoint("claw", tc.newVM, "127.0.0.1", 2222, "elasticclaw"); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.sshHostKeyCallback(addr)(addr, nil, trustedKey); err != nil {
				t.Fatalf("reconnect with trusted key: %v", err)
			}
			if err := s.sshHostKeyCallback(addr)(addr, nil, testSSHPublicKey(t)); err == nil {
				t.Fatal("reconnect with changed key should be rejected")
			}
		})
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
