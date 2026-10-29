package install

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func VerifyChecksum(r io.Reader, expected string) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return err
	}
	if actual := fmt.Sprintf("%x", h.Sum(nil)); actual != expected {
		return fmt.Errorf("SHA256 mismatch: got %s, expected %s", actual, expected)
	}
	return nil
}

// ExtractArchive rejects links and paths outside dest. Releases only need regular files and directories.
func ExtractArchive(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		path := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size > 2<<30 {
				return fmt.Errorf("archive entry too large: %s", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			mode := os.FileMode(0644)
			if h.Mode&0111 != 0 {
				mode = 0755
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry %q (type %d)", h.Name, h.Typeflag)
		}
	}
}

// SwapCurrent atomically replaces the stable link, leaving the old link intact on failure.
func SwapCurrent(link, target string) error {
	if _, err := os.Stat(target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(link), ".link-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	next := filepath.Join(temp, "current")
	if err := os.Symlink(target, next); err != nil {
		return err
	}
	return os.Rename(next, link)
}

func (i *Installer) installComponent(ctx context.Context, c Component) error {
	dest := i.path(BinaryDir, c.Name+"-"+c.Version)
	// Verified versions are immutable; reuse them on an idempotent install.
	sum := c.AMD64
	if i.Arch == "arm64" {
		sum = c.ARM64
	}
	verified, markerErr := os.ReadFile(filepath.Join(dest, ".verified"))
	if markerErr == nil && strings.TrimSpace(string(verified)) != sum {
		return fmt.Errorf("verification marker mismatch for %s", dest)
	}
	if markerErr != nil {
		staging, err := os.MkdirTemp(i.path(BinaryDir), ".download-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(c.URL, i.Arch), nil)
		if err != nil {
			return err
		}
		resp, err := i.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download %s: HTTP %d", c.Name, resp.StatusCode)
		}
		archive, err := os.Create(filepath.Join(staging, "archive.tar.gz"))
		if err != nil {
			return err
		}
		defer archive.Close()
		if _, err := io.Copy(archive, io.LimitReader(resp.Body, (2<<30)+1)); err != nil {
			return err
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := VerifyChecksum(archive, sum); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return err
		}
		unpacked := filepath.Join(staging, "unpacked")
		if err := os.MkdirAll(unpacked, 0755); err != nil {
			return err
		}
		if err := ExtractArchive(archive, unpacked); err != nil {
			return err
		}
		source := unpacked
		if c.Name == "grafana" {
			entries, err := os.ReadDir(unpacked)
			if err != nil {
				return err
			}
			if len(entries) != 1 || !entries[0].IsDir() {
				return fmt.Errorf("Grafana archive must contain one top-level directory")
			}
			source = filepath.Join(unpacked, entries[0].Name())
		}
		info, err := os.Stat(filepath.Join(source, c.Binary))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return fmt.Errorf("missing executable %s", c.Binary)
		}
		if err := os.WriteFile(filepath.Join(source, ".verified"), []byte(sum+"\n"), 0644); err != nil {
			return err
		}
		// Never replace a populated version directory behind a running service.
		if err := os.Rename(source, dest); err != nil {
			return err
		}
	}
	info, err := os.Stat(filepath.Join(dest, c.Binary))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return fmt.Errorf("missing executable %s", c.Binary)
	}
	return SwapCurrent(i.path(BinaryDir, c.Name, "current"), dest)
}
