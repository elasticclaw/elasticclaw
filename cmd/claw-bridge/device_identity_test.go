package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// seedDeviceIdentity writes a primary device identity row into the state DB
// exactly as the OpenClaw gateway would, for tests that need a pre-existing
// identity.
func seedDeviceIdentity(t *testing.T, home, deviceID, publicPEM, privatePEM string) {
	t.Helper()
	dbPath := filepath.Join(home, ".openclaw", "state", "openclaw.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(deviceIdentitySchema); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`INSERT INTO device_identities
		(identity_key, device_id, public_key_pem, private_key_pem, created_at_ms, updated_at_ms)
		VALUES(?,?,?,?,?,?) ON CONFLICT(identity_key) DO UPDATE SET
		device_id=excluded.device_id, public_key_pem=excluded.public_key_pem,
		private_key_pem=excluded.private_key_pem, updated_at_ms=excluded.updated_at_ms`,
		primaryDeviceIdentityKey, deviceID, publicPEM, privatePEM, now, now); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
}

func TestLoadOrCreateDeviceIdentityReadsExistingRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCLAW_STATE_DIR", "")
	seedDeviceIdentity(t, home, "device-existing", testPEM("PUBLIC KEY"), testPEM("PRIVATE KEY"))

	dev, err := loadOrCreateDeviceIdentity(t.Context())
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}
	if dev.DeviceID != "device-existing" {
		t.Fatalf("deviceId = %q, want device-existing", dev.DeviceID)
	}
}

func TestLoadOrCreateDeviceIdentityGeneratesAndPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCLAW_STATE_DIR", "")

	dev, err := loadOrCreateDeviceIdentity(t.Context())
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	assertDeviceIdentityPair(t, dev)

	// A second load must return the persisted row, not a fresh identity.
	again, err := loadOrCreateDeviceIdentity(t.Context())
	if err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	if again.DeviceID != dev.DeviceID || again.PrivateKeyPem != dev.PrivateKeyPem {
		t.Fatal("second load did not return the persisted identity")
	}
}

// assertDeviceIdentityPair verifies the full gateway handshake contract for a
// generated identity: the PEMs are a matching SPKI/PKCS8 Ed25519 pair, the
// deviceId is the SHA-256 hex digest of the raw public key (exactly what the
// gateway checks), and the production signing helper produces a signature
// that verifies with the public key.
func assertDeviceIdentityPair(t *testing.T, dev *deviceIdentity) {
	t.Helper()
	if len(dev.DeviceID) != 64 || strings.ToLower(dev.DeviceID) != dev.DeviceID {
		t.Fatalf("deviceId = %q, want 64-char lowercase sha256 hex", dev.DeviceID)
	}
	privBlock, _ := pem.Decode([]byte(dev.PrivateKeyPem))
	if privBlock == nil {
		t.Fatalf("private key PEM = %q, want PKCS8 PEM", dev.PrivateKeyPem)
	}
	priv, err := x509.ParsePKCS8PrivateKey(privBlock.Bytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	privEd, ok := priv.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("private key = %T, want ed25519.PrivateKey", priv)
	}
	pubBlock, _ := pem.Decode([]byte(dev.PublicKeyPem))
	if pubBlock == nil {
		t.Fatalf("public key PEM = %q, want SPKI PEM", dev.PublicKeyPem)
	}
	pubAny, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	pub, ok := pubAny.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("public key = %T, want ed25519.PublicKey", pubAny)
	}
	if !privEd.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("private key does not match the stored public key")
	}
	digest := sha256.Sum256(pub)
	if dev.DeviceID != hex.EncodeToString(digest[:]) {
		t.Fatalf("deviceId = %q, want sha256(public key) = %q", dev.DeviceID, hex.EncodeToString(digest[:]))
	}
	payload := []byte("handshake-payload")
	sig, err := ed25519Sign(dev.PrivateKeyPem, payload)
	if err != nil {
		t.Fatalf("sign with production helper: %v", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(pub, payload, sigBytes) {
		t.Fatal("signature from production helper does not verify with the stored public key")
	}
}

func TestLoadOrCreateDeviceIdentityRefusesLegacyJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCLAW_STATE_DIR", "")
	legacy := filepath.Join(home, ".openclaw", "identity", "device.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"deviceId":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := loadOrCreateDeviceIdentity(t.Context())
	if err == nil || !strings.Contains(err.Error(), "doctor --fix") {
		t.Fatalf("error = %v, want legacy identity refusal pointing at doctor --fix", err)
	}
}

func TestIsSessionAdmissionConflictError(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"message":"This session still has active or queued work. Wait for it to finish, then retry the Goal.","code":"goal-session-busy"}}`: true,
		`Session "agent:main:dashboard:x" changed while starting work. Retry.`:                                                                         true,
		`{"reason":"session-routing-changed","message":"session routing changed; review and retry"}`:                                                   true,
		`{"reason":"active-leaf-changed","message":"active branch changed; review and retry"}`:                                                         true,
		`Session settings changed before send. Retry.`:                                                                                                 true,
		// Unrelated file-edit conflict must not be treated as admission retry.
		`session file changed since it was read (session_file_conflict)`: false,
		`some other gateway error`:                                       false,
	}
	for msg, want := range cases {
		var err error
		if !want {
			// still exercise the matcher on the text
			err = errors.New(msg)
		} else {
			err = errors.New(msg)
		}
		if got := isSessionAdmissionConflictError(err); got != want {
			t.Fatalf("isSessionAdmissionConflictError(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestDeviceIdentitySchemaMatchesUpstreamShape(t *testing.T) {
	// The DDL must stay byte-compatible with OpenClaw's
	// openclaw-state-schema.sql so gateway-created rows and bridge-created
	// rows coexist in the same table.
	for _, want := range []string{
		"identity_key TEXT NOT NULL PRIMARY KEY",
		"device_id TEXT NOT NULL",
		"public_key_pem TEXT NOT NULL",
		"private_key_pem TEXT NOT NULL",
		"created_at_ms INTEGER NOT NULL",
		"updated_at_ms INTEGER NOT NULL",
		"STRICT",
		"idx_device_identities_device",
	} {
		if !strings.Contains(deviceIdentitySchema, want) {
			t.Fatalf("device identity schema missing %q", want)
		}
	}
}

func TestOpenClawStateDBPathHonorsStateDir(t *testing.T) {
	t.Setenv("OPENCLAW_STATE_DIR", "/tmp/state-override")
	path, err := openClawStateDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join("/tmp/state-override", "state", "openclaw.sqlite") {
		t.Fatalf("path = %q", path)
	}
}

var _ = json.Marshal // retain json import if cases change

// TestNodeCompatibleExprMatchesOpenClawEngines exercises the exact JS
// expression the bootstrap script embeds, with injected Node versions,
// against OpenClaw 2026.9.x's engines clause ">=24.16.0 <25 || >=26.1.0".
func TestNodeCompatibleExprMatchesOpenClawEngines(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not in PATH")
	}
	cases := []struct {
		version string
		want    bool
	}{
		{"24.15.0", false}, // below the 24.x floor
		{"24.16.0", true},  // floor
		{"24.21.0", true},  // Daytona images
		{"25.0.0", false},  // 25.x is excluded outright
		{"25.9.1", false},
		{"26.0.0", false}, // below the 26.x floor
		{"26.1.0", true},  // floor
		{"27.0.0", true},  // engines range is unbounded above 26.1
		{"28.3.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			script := "const [major, minor] = process.argv[1].split(\".\").map(Number);" +
				"process.exit(" + nodeCompatibleExpr + " ? 0 : 1)"
			cmd := exec.Command("node", "-e", script, tc.version)
			err := cmd.Run()
			if tc.want && err != nil {
				t.Fatalf("node %s rejected, want accepted (exit=%v)", tc.version, err)
			}
			if !tc.want && err == nil {
				t.Fatalf("node %s accepted, want rejected", tc.version)
			}
		})
	}
}

var _ = context.Background
