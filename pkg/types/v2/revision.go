package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ContentDigest is an immutable content-addressed revision of a v2 document.
type ContentDigest string

// RevisionFromCanonicalYAML returns a stable SHA-256 digest of the given
// canonical YAML bytes. Callers should pass CanonicalYAML output so equivalent
// documents share a revision.
func RevisionFromCanonicalYAML(canonical []byte) ContentDigest {
	sum := sha256.Sum256(canonical)
	return ContentDigest(hex.EncodeToString(sum[:]))
}

// CanonicalYAML re-marshals a validated document to YAML for hashing.
// yaml.v3 map key order is insertion order for map[string]T in recent Go;
// we marshal through a round-trip of the already-parsed typed structs so
// two Parse→Validate paths of the same authored document share a digest.
func CanonicalYAML(doc interface{}) ([]byte, error) {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("canonical yaml: %w", err)
	}
	return data, nil
}

// RevisionOf returns the content digest for a validated v2 document value.
func RevisionOf(doc interface{}) (ContentDigest, error) {
	canonical, err := CanonicalYAML(doc)
	if err != nil {
		return "", err
	}
	return RevisionFromCanonicalYAML(canonical), nil
}
