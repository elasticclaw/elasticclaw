package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type upgradeTransport func(*http.Request) (*http.Response, error)

func (f upgradeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestUpgradeMissingChecksum(t *testing.T) {
	oldVersion, oldClient := Version, http.DefaultClient
	t.Cleanup(func() { Version = oldVersion; http.DefaultClient = oldClient })
	Version = "2026.11.19"
	binaryRequested := false
	http.DefaultClient = &http.Client{Transport: upgradeTransport(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, ""
		switch {
		case req.URL.Host == "api.github.com":
			body = "[]"
			if req.URL.Query().Get("page") == "1" {
				body = `[{"tag_name":"2026.11.19.1"}]`
			}
		case strings.HasSuffix(req.URL.Path, "/checksums.txt"):
			body = strings.Repeat("a", 64) + "  other-asset\n"
		case strings.HasSuffix(req.URL.Path, ".minisig"):
			status = http.StatusNotFound
		default:
			binaryRequested = true
			return nil, fmt.Errorf("unexpected binary request: %s", req.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	command := &cobra.Command{}
	command.SetContext(context.Background())
	err := runUpgrade(command, nil)
	if err == nil || !strings.Contains(err.Error(), "missing checksum for elasticclaw-") {
		t.Fatalf("got %v", err)
	}
	if binaryRequested {
		t.Fatal("downloaded binary without its checksum")
	}
}
