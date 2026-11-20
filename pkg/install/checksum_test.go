package install

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChecksums(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("asset")))
	got, err := ParseChecksums([]byte(strings.ToUpper(digest) + "  asset name\n" + digest + " *binary\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got["asset name"] != digest || got["binary"] != digest {
		t.Fatalf("unexpected checksums: %v", got)
	}
	for _, input := range []string{"", "garbage", digest + " name", digest + "  ", strings.Repeat("z", 64) + "  name", digest + "  name\n" + digest + " *name", digest + "  name\r"} {
		if _, err := ParseChecksums([]byte(input)); err == nil {
			t.Errorf("accepted malformed checksums %q", input)
		}
	}
}

func TestVerifySHA256(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("asset")))
	if err := VerifySHA256(strings.NewReader("asset"), digest); err != nil {
		t.Fatal(err)
	}
	if err := VerifySHA256(strings.NewReader("modified"), digest); err == nil {
		t.Fatal("accepted mismatch")
	}
	if err := VerifySHA256(strings.NewReader("asset"), "bad"); err == nil {
		t.Fatal("accepted invalid digest")
	}
	filename := filepath.Join(t.TempDir(), "asset")
	if err := os.WriteFile(filename, []byte("asset"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFileSHA256(filename, digest); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFileSHA256(filename, strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted file mismatch")
	}
	if err := VerifyFileSHA256(filename+"missing", digest); err == nil {
		t.Fatal("accepted missing file")
	}
}

func TestFetchReleaseChecksums(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("asset")))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release/checksums.txt":
			fmt.Fprintf(w, "%s  asset\n", digest)
		case "/large/checksums.txt":
			io.WriteString(w, strings.Repeat("x", (1<<20)+1))
		case "/redirect/checksums.txt":
			http.Redirect(w, r, "http://example.com/checksums.txt", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	client := &http.Client{Transport: checksumTransport(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Result(), nil
	})}
	const baseURL = "http://localhost"
	got, err := FetchReleaseChecksums(context.Background(), client, baseURL+"/release")
	if err != nil || got["asset"] != digest {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, base := range []string{baseURL + "/large", baseURL + "/missing", baseURL + "/redirect", "http://example.com/release", "ftp://localhost/release"} {
		if _, err := FetchReleaseChecksums(context.Background(), client, base); err == nil {
			t.Errorf("accepted %s", base)
		}
	}
}

type checksumTransport func(*http.Request) (*http.Response, error)

func (f checksumTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
