package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

func signedMinisign(t *testing.T, message []byte, algorithm string) (string, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := []byte("test-key")
	key := append(append([]byte("Ed"), id...), public...)
	payload := message
	if algorithm == "ED" {
		digest := blake2b.Sum512(message)
		payload = digest[:]
	}
	sig := ed25519.Sign(private, payload)
	packet := append(append([]byte(algorithm), id...), sig...)
	const comment = "elasticclaw test"
	global := ed25519.Sign(private, append(append([]byte(nil), sig...), []byte(comment)...))
	return base64.StdEncoding.EncodeToString(key), []byte(fmt.Sprintf("untrusted comment: test signature\n%s\ntrusted comment: %s\n%s\n", base64.StdEncoding.EncodeToString(packet), comment, base64.StdEncoding.EncodeToString(global)))
}

func TestVerifyMinisign(t *testing.T) {
	for _, algorithm := range []string{"Ed", "ED"} {
		t.Run(algorithm, func(t *testing.T) {
			message := []byte("release checksums\n")
			key, signature := signedMinisign(t, message, algorithm)
			if err := VerifyMinisign(message, signature, "untrusted comment: test key\n"+key); err != nil {
				t.Fatal(err)
			}
			wrongID, _ := base64.StdEncoding.DecodeString(key)
			wrongID[2] ^= 1
			cases := []struct {
				name               string
				message, signature []byte
				key                string
			}{
				{"message", []byte("tampered"), signature, key},
				{"key ID", message, signature, base64.StdEncoding.EncodeToString(wrongID)},
				{"trusted comment", message, []byte(strings.Replace(string(signature), "elasticclaw test", "elasticclaw evil", 1)), key},
				{"missing global", message, []byte(strings.Join(strings.Split(string(signature), "\n")[:3], "\n")), key},
				{"invalid key", message, signature, "bad"},
				{"invalid packet", message, []byte("untrusted comment: test\nbad\ntrusted comment: test\nbad\n"), key},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if err := VerifyMinisign(tc.message, tc.signature, tc.key); err == nil {
						t.Fatal("accepted tampered signature")
					}
				})
			}
		})
	}
}

func TestFetchSignedReleaseChecksums(t *testing.T) {
	message := []byte(strings.Repeat("a", 64) + "  asset\n")
	key, signature := signedMinisign(t, message, "ED")
	for _, mode := range []string{"valid", "missing", "invalid", "tampered"} {
		t.Run(mode, func(t *testing.T) {
			client := &http.Client{Transport: checksumTransport(func(req *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				if strings.HasSuffix(req.URL.Path, ".minisig") {
					switch mode {
					case "missing":
						w.WriteHeader(http.StatusNotFound)
					case "invalid":
						w.Write([]byte("invalid"))
					default:
						w.Write(signature)
					}
				} else if mode == "tampered" {
					w.Write([]byte(strings.Repeat("b", 64) + "  asset\n"))
				} else {
					w.Write(message)
				}
				return w.Result(), nil
			})}
			got, err := fetchReleaseChecksums(context.Background(), client, "https://example.com/release", key)
			if mode == "valid" {
				if err != nil || got["asset"] != strings.Repeat("a", 64) {
					t.Fatalf("got %v, %v", got, err)
				}
			} else if err == nil {
				t.Fatal("accepted unverified checksums")
			}
		})
	}
}
