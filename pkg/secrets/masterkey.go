package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MasterKeyEnvVar holds the base64-encoded 32-byte master key when set.
const MasterKeyEnvVar = "ELASTICCLAW_MASTER_KEY"

// MasterKeySize is the AES-256 key size in bytes.
const MasterKeySize = 32

// masterKeyFileName is the on-disk key file created on first boot.
const masterKeyFileName = "master.key"

// ErrNoMasterKey indicates no master key is available from env or disk.
var ErrNoMasterKey = errors.New("no master key available: set " + MasterKeyEnvVar + " or restore the master.key file")

// NewMasterKey generates a fresh random 32-byte key, base64-encoded.
func NewMasterKey() (string, error) {
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("failed to generate master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ParseMasterKey decodes a base64 master key and validates its length.
func ParseMasterKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("master key is not valid base64: %w", err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes after base64 decoding, got %d", MasterKeySize, len(key))
	}
	return key, nil
}

// MasterKeyFilePath returns the path of the master key file:
//   - next to the hub config when ELASTICCLAW_HUB_CONFIG points at a custom path
//   - ~/.elasticclaw/master.key otherwise
func MasterKeyFilePath() (string, error) {
	if cfgPath := os.Getenv("ELASTICCLAW_HUB_CONFIG"); cfgPath != "" {
		return filepath.Join(filepath.Dir(cfgPath), masterKeyFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine master key path: %w", err)
	}
	return filepath.Join(home, ".elasticclaw", masterKeyFileName), nil
}

// LoadMasterKey returns the master key from ELASTICCLAW_MASTER_KEY or the
// master.key file. It returns ErrNoMasterKey (wrapped) when neither exists.
func LoadMasterKey() ([]byte, error) {
	if env := os.Getenv(MasterKeyEnvVar); env != "" {
		return ParseMasterKey(env)
	}
	path, err := MasterKeyFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w (looked for %s)", ErrNoMasterKey, path)
		}
		return nil, fmt.Errorf("failed to read master key file %s: %w", path, err)
	}
	key, err := ParseMasterKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("invalid master key file %s: %w", path, err)
	}
	return key, nil
}

// EnsureMasterKey returns the master key, generating and persisting a new
// master.key file (0600) on first use when no key exists yet.
func EnsureMasterKey() ([]byte, error) {
	key, err := LoadMasterKey()
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, ErrNoMasterKey) {
		return nil, err
	}
	path, err := MasterKeyFilePath()
	if err != nil {
		return nil, err
	}
	encoded, err := NewMasterKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("failed to create master key directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("failed to write master key file %s: %w", path, err)
	}
	return ParseMasterKey(encoded)
}
