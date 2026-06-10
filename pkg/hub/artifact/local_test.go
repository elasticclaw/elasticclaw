package artifact

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLocalStoreBlobManifestAndRef(t *testing.T) {
	ctx := context.Background()
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}

	digest, size, err := store.PutBlob(ctx, strings.NewReader("hello artifacts"))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	if size != int64(len("hello artifacts")) {
		t.Fatalf("size = %d, want %d", size, len("hello artifacts"))
	}
	if digest != DigestBytes([]byte("hello artifacts")) {
		t.Fatalf("digest = %q, want content digest", digest)
	}

	rc, err := store.GetBlob(ctx, digest)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	got, err := io.ReadAll(rc)
	if closeErr := rc.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != "hello artifacts" {
		t.Fatalf("blob = %q", got)
	}

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	manifestDigest, err := store.PutManifest(ctx, manifest)
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	loaded, err := store.GetManifest(ctx, manifestDigest)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	if string(loaded) != string(manifest) {
		t.Fatalf("manifest = %s, want %s", loaded, manifest)
	}

	if err := store.Tag(ctx, "volumes/engineering/cache", "latest", manifestDigest); err != nil {
		t.Fatalf("tag: %v", err)
	}
	resolved, err := store.ResolveRef(ctx, "volumes/engineering/cache", "latest")
	if err != nil {
		t.Fatalf("resolve ref: %v", err)
	}
	if resolved != manifestDigest {
		t.Fatalf("resolved = %q, want %q", resolved, manifestDigest)
	}
}

func TestLocalStoreRejectsInvalidRefs(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	for _, tc := range []struct {
		repo string
		tag  string
	}{
		{"../escape", "latest"},
		{"/absolute", "latest"},
		{"volumes//cache", "latest"},
		{"volumes/cache", "../latest"},
		{"volumes/cache", "release/latest"},
	} {
		if err := store.Tag(context.Background(), tc.repo, tc.tag, DigestBytes([]byte("manifest"))); err == nil {
			t.Fatalf("Tag(%q, %q) succeeded, want error", tc.repo, tc.tag)
		}
	}
}

func TestLocalStoreDetectsCorruptManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	digest, err := store.PutManifest(ctx, []byte("original"))
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	_, encoded, err := ParseDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "manifests", "sha256", encoded[:2], encoded[2:4], encoded)
	if err := os.WriteFile(path, []byte("corrupt"), 0o640); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}
	if _, err := store.GetManifest(ctx, digest); err == nil {
		t.Fatal("expected corrupt manifest digest error")
	}
}

func TestNewStoreFromHubConfigDefaultsToLocalArtifactsUnderDataDir(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStoreFromHubConfig(context.Background(), dataDir, nil)
	if err != nil {
		t.Fatalf("new store from nil config: %v", err)
	}
	if _, ok := store.(*LocalStore); !ok {
		t.Fatalf("store type = %T, want *LocalStore", store)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "artifacts", "blobs", "sha256")); err != nil {
		t.Fatalf("default local artifact dir not created: %v", err)
	}
}

func TestNewStoreFromHubConfigRejectsMissingS3Config(t *testing.T) {
	_, err := NewStoreFromHubConfig(context.Background(), t.TempDir(), &types.ArtifactStorageConfig{Backend: "s3"})
	if err == nil {
		t.Fatal("expected missing s3 config error")
	}
}
