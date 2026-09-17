package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// OpenClaw 2026.9.x stores the primary device identity in the shared state
// SQLite database (src/infra/device-identity-store.ts) instead of the retired
// ~/.openclaw/identity/device.json. The DDL below mirrors
// openclaw-state-schema.sql so a bridge-created row is byte-compatible with
// rows the gateway itself writes.
const deviceIdentitySchema = `
CREATE TABLE IF NOT EXISTS device_identities (
  identity_key TEXT NOT NULL PRIMARY KEY,
  device_id TEXT NOT NULL,
  public_key_pem TEXT NOT NULL,
  private_key_pem TEXT NOT NULL,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_device_identities_device
  ON device_identities(device_id, updated_at_ms DESC);
`

const primaryDeviceIdentityKey = "primary"

// openClawStateDBPath returns the shared state database path, honoring
// OPENCLAW_STATE_DIR like OpenClaw's resolveStateDir.
func openClawStateDBPath() (string, error) {
	root := os.Getenv("OPENCLAW_STATE_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		root = filepath.Join(home, ".openclaw")
	}
	return filepath.Join(root, "state", "openclaw.sqlite"), nil
}

// loadOrCreateDeviceIdentity returns the primary device identity from the
// shared state database, generating and inserting it when absent. The insert
// races safely with the gateway doing the same: ON CONFLICT DO NOTHING, then
// the authoritative row is re-read (mirrors upstream's
// insertStoredDeviceIdentityIfAbsent).
func loadOrCreateDeviceIdentity(ctx context.Context) (*deviceIdentity, error) {
	dbPath, err := openClawStateDBPath()
	if err != nil {
		return nil, err
	}
	// A retired 2026.7.x identity file makes upstream clients refuse to start;
	// match that behavior instead of silently forking identities.
	home, _ := os.UserHomeDir()
	if legacy := filepath.Join(home, ".openclaw", "identity", "device.json"); home != "" {
		if _, err := os.Stat(legacy); err == nil {
			return nil, fmt.Errorf("legacy device identity exists at %s; run `openclaw doctor --fix` before connecting", legacy)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, deviceIdentitySchema); err != nil {
		return nil, fmt.Errorf("ensure device_identities schema: %w", err)
	}

	read := func() (*deviceIdentity, error) {
		var dev deviceIdentity
		err := db.QueryRowContext(ctx, `SELECT device_id, public_key_pem, private_key_pem
			FROM device_identities WHERE identity_key=?`, primaryDeviceIdentityKey).
			Scan(&dev.DeviceID, &dev.PublicKeyPem, &dev.PrivateKeyPem)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read device identity: %w", err)
		}
		return &dev, nil
	}

	existing, err := read()
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	generated, err := generateDeviceIdentity()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO device_identities
		(identity_key, device_id, public_key_pem, private_key_pem, created_at_ms, updated_at_ms)
		VALUES(?,?,?,?,?,?) ON CONFLICT(identity_key) DO NOTHING`,
		primaryDeviceIdentityKey, generated.DeviceID, generated.PublicKeyPem,
		generated.PrivateKeyPem, now, now); err != nil {
		return nil, fmt.Errorf("insert device identity: %w", err)
	}
	// A concurrent creator may have won the insert; its row is authoritative.
	authoritative, err := read()
	if err != nil {
		return nil, err
	}
	if authoritative != nil {
		return authoritative, nil
	}
	return generated, nil
}

// generateDeviceIdentity creates a fresh Ed25519 device identity in the exact
// shapes OpenClaw stores: SPKI public PEM, PKCS8 private PEM, and
// deviceId = hex(sha256(raw 32-byte public key)).
func generateDeviceIdentity() (*deviceIdentity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	digest := sha256.Sum256(pub)
	return &deviceIdentity{
		DeviceID:      hex.EncodeToString(digest[:]),
		PublicKeyPem:  string(pubPEM),
		PrivateKeyPem: string(privPEM),
	}, nil
}
