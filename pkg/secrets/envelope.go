// Package secrets implements encryption at rest for sensitive hub.yaml values.
//
// Sensitive values are stored as an envelope string:
//
//	enc:v1:<base64(nonce || ciphertext)>
//
// encrypted with AES-256-GCM under a 32-byte master key. The master key comes
// from the ELASTICCLAW_MASTER_KEY environment variable (base64) or from a
// master.key file created on first boot (0600).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// EnvelopePrefix marks a value encrypted with the v1 scheme (AES-256-GCM).
const EnvelopePrefix = "enc:v1:"

// envelopeMarker is the generic prefix reserved for encrypted values of any version.
const envelopeMarker = "enc:"

// IsEncrypted reports whether a value carries an encryption envelope
// (any version, not just v1).
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, envelopeMarker)
}

// Encrypt seals plaintext with AES-256-GCM and returns an enc:v1: envelope.
func Encrypt(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return EnvelopePrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens an enc:v1: envelope and returns the plaintext.
// Values without an enc: prefix are returned unchanged (plaintext passthrough,
// used for transparent migration of pre-encryption configs).
func Decrypt(key []byte, value string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	if !strings.HasPrefix(value, EnvelopePrefix) {
		return "", fmt.Errorf("unsupported secret envelope version (expected %q prefix)", EnvelopePrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, EnvelopePrefix))
	if err != nil {
		return "", fmt.Errorf("invalid secret envelope encoding: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("invalid secret envelope: too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt secret (wrong master key?): %w", err)
	}
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", MasterKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
