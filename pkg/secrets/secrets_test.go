package secrets

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	encoded, err := NewMasterKey()
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	key, err := ParseMasterKey(encoded)
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	return key
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := testKey(t)
	for _, plaintext := range []string{"hunter2", "", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"} {
		env, err := Encrypt(key, plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plaintext, err)
		}
		if !strings.HasPrefix(env, EnvelopePrefix) {
			t.Fatalf("envelope %q missing prefix", env)
		}
		if strings.Contains(env, plaintext) && plaintext != "" {
			t.Fatalf("envelope leaks plaintext: %q", env)
		}
		got, err := Decrypt(key, env)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plaintext {
			t.Fatalf("roundtrip = %q, want %q", got, plaintext)
		}
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	got, err := Decrypt(testKey(t), "plain-value")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "plain-value" {
		t.Fatalf("got %q, want plaintext passthrough", got)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	env, err := Encrypt(testKey(t), "secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := Decrypt(testKey(t), env); err == nil {
		t.Fatal("expected decrypt with wrong key to fail")
	}
}

func TestDecryptUnknownVersionFails(t *testing.T) {
	if _, err := Decrypt(testKey(t), "enc:v9:abcd"); err == nil {
		t.Fatal("expected unknown envelope version to fail")
	}
}

type nestedSecret struct {
	Token  string `secret:"true"`
	Public string
}

type testConfig struct {
	Name     string
	Password string            `secret:"true"`
	Secrets  map[string]string `secret:"true"`
	Nested   *nestedSecret
	List     []*nestedSecret
	ByName   map[string]nestedSecret
}

func TestEncryptDecryptFields(t *testing.T) {
	key := testKey(t)
	cfg := &testConfig{
		Name:     "visible",
		Password: "p4ss",
		Secrets:  map[string]string{"a": "secret-a", "empty": ""},
		Nested:   &nestedSecret{Token: "tok-1", Public: "pub"},
		List:     []*nestedSecret{{Token: "tok-2"}, nil},
		ByName:   map[string]nestedSecret{"x": {Token: "tok-3"}},
	}
	if err := EncryptFields(cfg, key); err != nil {
		t.Fatalf("EncryptFields: %v", err)
	}
	if cfg.Name != "visible" || cfg.Nested.Public != "pub" {
		t.Fatal("non-secret fields must be untouched")
	}
	for desc, v := range map[string]string{
		"Password":        cfg.Password,
		"Secrets[a]":      cfg.Secrets["a"],
		"Nested.Token":    cfg.Nested.Token,
		"List[0].Token":   cfg.List[0].Token,
		"ByName[x].Token": cfg.ByName["x"].Token,
	} {
		if !IsEncrypted(v) {
			t.Fatalf("%s = %q, want encrypted", desc, v)
		}
	}
	if cfg.Secrets["empty"] != "" {
		t.Fatal("empty values must stay empty")
	}
	if !ContainsEncrypted(cfg) {
		t.Fatal("ContainsEncrypted = false, want true")
	}

	// Idempotence: encrypting twice must not double-wrap.
	once := cfg.Password
	if err := EncryptFields(cfg, key); err != nil {
		t.Fatalf("EncryptFields (2nd): %v", err)
	}
	if cfg.Password != once {
		t.Fatal("EncryptFields is not idempotent")
	}

	if err := DecryptFields(cfg, key); err != nil {
		t.Fatalf("DecryptFields: %v", err)
	}
	if cfg.Password != "p4ss" || cfg.Secrets["a"] != "secret-a" ||
		cfg.Nested.Token != "tok-1" || cfg.List[0].Token != "tok-2" ||
		cfg.ByName["x"].Token != "tok-3" {
		t.Fatalf("decrypt mismatch: %+v", cfg)
	}
	if ContainsEncrypted(cfg) {
		t.Fatal("ContainsEncrypted = true after decrypt, want false")
	}
}

func TestMasterKeyFromEnv(t *testing.T) {
	encoded, err := NewMasterKey()
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	t.Setenv(MasterKeyEnvVar, encoded)
	key, err := LoadMasterKey()
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if base64.StdEncoding.EncodeToString(key) != encoded {
		t.Fatal("env key mismatch")
	}
}

func TestEnsureMasterKeyCreatesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MasterKeyEnvVar, "")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(dir, "hub.yaml"))

	if _, err := LoadMasterKey(); err == nil {
		t.Fatal("expected LoadMasterKey to fail before EnsureMasterKey")
	}
	key1, err := EnsureMasterKey()
	if err != nil {
		t.Fatalf("EnsureMasterKey: %v", err)
	}
	path := filepath.Join(dir, "master.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat master.key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("master.key mode = %o, want 0600", perm)
	}
	// Second call reuses the same key.
	key2, err := EnsureMasterKey()
	if err != nil {
		t.Fatalf("EnsureMasterKey (2nd): %v", err)
	}
	if string(key1) != string(key2) {
		t.Fatal("EnsureMasterKey generated a different key on second call")
	}
}
