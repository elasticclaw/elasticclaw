package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestS3StoreBlobManifestAndRef(t *testing.T) {
	ctx := context.Background()
	client := &fakeS3HTTPClient{objects: map[string][]byte{}}
	store, err := NewS3Store(ctx, S3Config{
		Bucket:          "bucket",
		Region:          "us-east-1",
		Endpoint:        "http://s3.test",
		Prefix:          "elasticclaw",
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		PathStyle:       true,
		HTTPClient:      client,
	})
	if err != nil {
		t.Fatalf("new s3 store: %v", err)
	}

	digest, size, err := store.PutBlob(ctx, strings.NewReader("hello s3"))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	if size != int64(len("hello s3")) {
		t.Fatalf("size = %d, want %d", size, len("hello s3"))
	}
	rc, err := store.GetBlob(ctx, digest)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	data, err := io.ReadAll(rc)
	if closeErr := rc.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(data) != "hello s3" {
		t.Fatalf("blob = %q", data)
	}

	manifestDigest, err := store.PutManifest(ctx, []byte(`{"schemaVersion":2}`))
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	manifest, err := store.GetManifest(ctx, manifestDigest)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	if string(manifest) != `{"schemaVersion":2}` {
		t.Fatalf("manifest = %s", manifest)
	}
	if err := store.Tag(ctx, "checkpoints/claw-1", "latest", manifestDigest); err != nil {
		t.Fatalf("tag: %v", err)
	}
	resolved, err := store.ResolveRef(ctx, "checkpoints/claw-1", "latest")
	if err != nil {
		t.Fatalf("resolve ref: %v", err)
	}
	if resolved != manifestDigest {
		t.Fatalf("resolved = %q, want %q", resolved, manifestDigest)
	}
}

type fakeS3HTTPClient struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (c *fakeS3HTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch req.Method {
	case http.MethodPut:
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		c.objects[req.URL.Path] = data
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	case http.MethodGet:
		data, ok := c.objects[req.URL.Path]
		if !ok {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Length": {fmt.Sprintf("%d", len(data))}},
			Body:       io.NopCloser(bytes.NewReader(data)),
		}, nil
	default:
		return &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("method not allowed")),
		}, nil
	}
}
