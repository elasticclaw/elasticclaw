package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// HubIdentity holds the hub's SSH keypair used to access provisioned VMs.
type HubIdentity struct {
	PrivateKey ssh.Signer
	PublicKey  string // authorized_keys format, sent to providers
}

// LoadOrCreateIdentity loads the hub's keypair from disk, creating it if absent.
// The keypair is stored at <dir>/hub-key (private) and <dir>/hub-key.pub (public).
func LoadOrCreateIdentity(dir string) (*HubIdentity, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create identity dir: %w", err)
	}

	privPath := filepath.Join(dir, "hub-key")
	pubPath := filepath.Join(dir, "hub-key.pub")

	// If both files exist, load them
	if _, err := os.Stat(privPath); err == nil {
		return loadIdentity(privPath, pubPath)
	}

	// Generate new ed25519 keypair
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	// Marshal private key as PEM
	privPEM, err := ssh.MarshalPrivateKey(priv, "elasticclaw hub key")
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	privBytes := pem.EncodeToMemory(privPEM)
	if err := os.WriteFile(privPath, privBytes, 0600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}

	// Marshal public key in authorized_keys format with a comment so providers can identify it
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	// MarshalAuthorizedKey produces "<type> <blob>\n"; append comment with username
	pubLine := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(sshPub)), "\n") + " elasticclaw@hub\n"
	if err := os.WriteFile(pubPath, []byte(pubLine), 0644); err != nil {
		return nil, fmt.Errorf("write public key: %w", err)
	}

	return loadIdentity(privPath, pubPath)
}

func loadIdentity(privPath, pubPath string) (*HubIdentity, error) {
	privBytes, err := os.ReadFile(privPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(privBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	pubBytes, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}

	return &HubIdentity{
		PrivateKey: signer,
		PublicKey:  string(pubBytes),
	}, nil
}
