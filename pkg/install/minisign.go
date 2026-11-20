package install

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// ReleaseMinisignPublicKey will be filled in once the release signing key exists.
// Until then, checksum downloads emit a warning and skip signature verification.
const ReleaseMinisignPublicKey = ""

// VerifyMinisign verifies a minisign message signature and its trusted comment.
func VerifyMinisign(message, signature []byte, publicKey string) error {
	keyLines := strings.Split(strings.TrimSpace(publicKey), "\n")
	key, err := base64.StdEncoding.DecodeString(keyLines[len(keyLines)-1])
	if err != nil || len(key) != 42 || string(key[:2]) != "Ed" {
		return fmt.Errorf("invalid minisign public key")
	}
	lines := strings.Split(strings.TrimSuffix(string(signature), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "untrusted comment: ") || !strings.HasPrefix(lines[2], "trusted comment: ") {
		return fmt.Errorf("invalid minisign signature layout")
	}
	sig, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(sig) != 74 || (string(sig[:2]) != "Ed" && string(sig[:2]) != "ED") {
		return fmt.Errorf("invalid minisign signature")
	}
	if !bytes.Equal(key[2:10], sig[2:10]) {
		return fmt.Errorf("minisign key ID mismatch")
	}
	payload := message
	if string(sig[:2]) == "ED" {
		digest := blake2b.Sum512(message)
		payload = digest[:]
	}
	if !ed25519.Verify(ed25519.PublicKey(key[10:]), payload, sig[10:]) {
		return fmt.Errorf("invalid minisign message signature")
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return fmt.Errorf("invalid minisign global signature")
	}
	trusted := strings.TrimPrefix(lines[2], "trusted comment: ")
	signedComment := append(append([]byte(nil), sig[10:]...), []byte(trusted)...)
	if !ed25519.Verify(ed25519.PublicKey(key[10:]), signedComment, global) {
		return fmt.Errorf("invalid minisign trusted comment signature")
	}
	return nil
}
