package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

const (
	MediaTypeCheckpointV1      = "application/vnd.elasticclaw.checkpoint.v1+json"
	MediaTypeVolumeV1          = "application/vnd.elasticclaw.volume.v1+json"
	MediaTypeVolumeLayerTarZst = "application/vnd.elasticclaw.volume.layer.v1.tar+zstd"
)

type Store interface {
	PutBlob(ctx context.Context, r io.Reader) (digest string, size int64, err error)
	GetBlob(ctx context.Context, digest string) (io.ReadCloser, error)
	PutManifest(ctx context.Context, manifest []byte) (digest string, err error)
	GetManifest(ctx context.Context, digest string) ([]byte, error)
	ResolveRef(ctx context.Context, repo, tag string) (digest string, err error)
	Tag(ctx context.Context, repo, tag, digest string) error
}

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ParseDigest(digest string) (algorithm, encoded string, err error) {
	algorithm, encoded, ok := strings.Cut(strings.TrimSpace(digest), ":")
	if !ok {
		return "", "", fmt.Errorf("invalid digest %q", digest)
	}
	if algorithm != "sha256" {
		return "", "", fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	if len(encoded) != sha256.Size*2 {
		return "", "", fmt.Errorf("invalid sha256 digest length")
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", "", fmt.Errorf("invalid sha256 digest: %w", err)
	}
	return algorithm, strings.ToLower(encoded), nil
}

func ValidateRef(repo, tag string) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	if tag = strings.TrimSpace(tag); tag == "" {
		return fmt.Errorf("tag is empty")
	}
	if strings.ContainsAny(tag, `/\`) || strings.Contains(tag, "..") {
		return fmt.Errorf("tag %q contains path traversal or separators", tag)
	}
	return nil
}

func validateRepo(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fmt.Errorf("repo is empty")
	}
	if strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "\\") {
		return fmt.Errorf("repo %q must be relative", repo)
	}
	normalized := strings.ReplaceAll(repo, "\\", "/")
	for _, part := range strings.Split(normalized, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("repo %q contains invalid path segment", repo)
		}
	}
	return nil
}

type verifyingReadCloser struct {
	rc       io.ReadCloser
	hash     hash.Hash
	expected string
	done     bool
}

func newVerifyingReadCloser(rc io.ReadCloser, digest string) io.ReadCloser {
	return &verifyingReadCloser{rc: rc, hash: sha256.New(), expected: digest}
}

func (r *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
	}
	if err == io.EOF && !r.done {
		r.done = true
		got := "sha256:" + hex.EncodeToString(r.hash.Sum(nil))
		if got != r.expected {
			return n, fmt.Errorf("artifact digest mismatch: got %s, want %s", got, r.expected)
		}
	}
	return n, err
}

func (r *verifyingReadCloser) Close() error {
	return r.rc.Close()
}
