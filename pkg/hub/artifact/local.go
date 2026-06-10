package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type LocalStore struct {
	root string
}

func NewLocalStore(root string) (*LocalStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("artifact store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{
		filepath.Join(abs, "blobs", "sha256"),
		filepath.Join(abs, "manifests", "sha256"),
		filepath.Join(abs, "refs"),
		filepath.Join(abs, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return &LocalStore{root: abs}, nil
}

func (s *LocalStore) PutBlob(ctx context.Context, r io.Reader) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "blob-*")
	if err != nil {
		return "", 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(r, h))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	path, err := s.blobPath(digest)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", 0, err
	}
	if _, err := os.Stat(path); err == nil {
		return digest, size, nil
	} else if !os.IsNotExist(err) {
		return "", 0, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

func (s *LocalStore) GetBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.blobPath(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return newVerifyingReadCloser(f, digest), nil
}

func (s *LocalStore) PutManifest(ctx context.Context, manifest []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := DigestBytes(manifest)
	path, err := s.manifestPath(digest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return digest, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := s.writeFileAtomic(path, manifest, 0o640); err != nil {
		return "", err
	}
	return digest, nil
}

func (s *LocalStore) GetManifest(ctx context.Context, digest string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.manifestPath(digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if got := DigestBytes(data); got != digest {
		return nil, fmt.Errorf("artifact manifest digest mismatch: got %s, want %s", got, digest)
	}
	return data, nil
}

func (s *LocalStore) ResolveRef(ctx context.Context, repo, tag string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := s.refPath(repo, tag)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(string(data))
	if _, _, err := ParseDigest(digest); err != nil {
		return "", fmt.Errorf("invalid digest in ref %s:%s: %w", repo, tag, err)
	}
	return digest, nil
}

func (s *LocalStore) Tag(ctx context.Context, repo, tag, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := ParseDigest(digest); err != nil {
		return err
	}
	path, err := s.refPath(repo, tag)
	if err != nil {
		return err
	}
	return s.writeFileAtomic(path, []byte(digest+"\n"), 0o640)
}

func (s *LocalStore) blobPath(digest string) (string, error) {
	_, encoded, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	return digestPath(filepath.Join(s.root, "blobs", "sha256"), encoded), nil
}

func (s *LocalStore) manifestPath(digest string) (string, error) {
	_, encoded, err := ParseDigest(digest)
	if err != nil {
		return "", err
	}
	return digestPath(filepath.Join(s.root, "manifests", "sha256"), encoded), nil
}

func (s *LocalStore) refPath(repo, tag string) (string, error) {
	if err := ValidateRef(repo, tag); err != nil {
		return "", err
	}
	parts := append([]string{s.root, "refs"}, strings.Split(repo, "/")...)
	parts = append(parts, tag)
	return filepath.Join(parts...), nil
}

func (s *LocalStore) writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "artifact-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func digestPath(base, encoded string) string {
	return filepath.Join(base, encoded[:2], encoded[2:4], encoded)
}
