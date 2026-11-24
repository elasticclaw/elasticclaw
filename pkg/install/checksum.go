package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// ParseChecksums parses sha256sum text and returns lowercase digests by filename.
func ParseChecksums(data []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("malformed checksum on line %d", i+1)
		}
		digest, err := hex.DecodeString(line[:64])
		name := line[66:]
		if err != nil || len(digest) != sha256.Size || strings.ContainsAny(name, "\r\x00") {
			return nil, fmt.Errorf("malformed checksum on line %d", i+1)
		}
		if _, exists := checksums[name]; exists {
			return nil, fmt.Errorf("duplicate checksum for %q", name)
		}
		checksums[name] = strings.ToLower(line[:64])
	}
	if len(checksums) == 0 {
		return nil, fmt.Errorf("empty checksums")
	}
	return checksums, nil
}

func checksumURLAllowed(u *url.URL) bool {
	if u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback()))
}

// FetchReleaseChecksums fetches checksums.txt over HTTPS (HTTP is allowed on loopback).
func FetchReleaseChecksums(ctx context.Context, client *http.Client, baseURL string) (map[string]string, error) {
	return fetchReleaseChecksums(ctx, client, baseURL, ReleaseMinisignPublicKey)
}

// FetchUnsignedChecksums fetches checksums.txt without signature verification,
// for non-release sources such as preview builds that carry no release signature.
func FetchUnsignedChecksums(ctx context.Context, client *http.Client, baseURL string) (map[string]string, error) {
	data, err := fetchReleaseFile(ctx, client, strings.TrimRight(baseURL, "/")+"/checksums.txt")
	if err != nil {
		return nil, err
	}
	return ParseChecksums(data)
}

func fetchReleaseChecksums(ctx context.Context, client *http.Client, baseURL, publicKey string) (map[string]string, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	data, err := fetchReleaseFile(ctx, client, baseURL+"/checksums.txt")
	if err != nil {
		return nil, err
	}
	signature, sigErr := fetchReleaseFile(ctx, client, baseURL+"/checksums.txt.minisig")
	if publicKey == "" {
		log.Print("Warning: release signing key is not configured; skipping checksums.txt signature verification")
	} else {
		if sigErr != nil {
			return nil, fmt.Errorf("fetch checksums signature: %w", sigErr)
		}
		if err := VerifyMinisign(data, signature, publicKey); err != nil {
			return nil, fmt.Errorf("verify checksums signature: %w", err)
		}
	}
	return ParseChecksums(data)
}

func fetchReleaseFile(ctx context.Context, client *http.Client, fileURL string) ([]byte, error) {
	u, err := url.Parse(fileURL)
	if err != nil {
		return nil, err
	}
	if !checksumURLAllowed(u) {
		return nil, fmt.Errorf("checksums URL requires HTTPS: %s", u)
	}
	if client == nil {
		client = http.DefaultClient
	}
	checkedClient := *client
	checkedClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !checksumURLAllowed(req.URL) {
			return fmt.Errorf("checksums redirect requires HTTPS: %s", req.URL)
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many checksums redirects")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := checkedClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch checksums: HTTP %d", resp.StatusCode)
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("checksums exceed %d bytes", limit)
	}
	return data, nil
}

// VerifySHA256 checks a stream against a hexadecimal SHA256 digest.
func VerifySHA256(r io.Reader, want string) error {
	expected, err := hex.DecodeString(want)
	if err != nil || len(expected) != sha256.Size {
		return fmt.Errorf("invalid SHA256 digest %q", want)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, r); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		return fmt.Errorf("SHA256 mismatch: got %x, want %s", hash.Sum(nil), want)
	}
	return nil
}

// VerifyFileSHA256 checks a file against a hexadecimal SHA256 digest.
func VerifyFileSHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return VerifySHA256(f, want)
}

// ChecksumsBaseURL returns the directory URL that holds checksums.txt for a binary URL.
func ChecksumsBaseURL(binaryURL string) (string, error) {
	i := strings.LastIndex(binaryURL, "/")
	if i <= 0 || i == len(binaryURL)-1 {
		return "", fmt.Errorf("binary URL must include a path to a file: %q", binaryURL)
	}
	return binaryURL[:i], nil
}
