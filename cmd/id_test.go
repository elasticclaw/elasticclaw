package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "empty", id: "", want: ""},
		{name: "short", id: "abc", want: "abc"},
		{name: "exact", id: "abcdefgh", want: "abcdefgh"},
		{name: "long", id: "abcdefghi", want: "abcdefgh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortID(tt.id); got != tt.want {
				t.Fatalf("shortID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestRunListHubHandlesShortClawID(t *testing.T) {
	oldJSONOut := jsonOut
	oldListTag := listTag
	jsonOut = false
	listTag = ""
	defer func() {
		jsonOut = oldJSONOut
		listTag = oldListTag
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/claws" {
			http.NotFound(w, r)
			return
		}
		claws := []types.Claw{{
			ID:       "abc",
			Name:     "short",
			Template: "elasticclaw",
			Status:   types.StatusRunning,
		}}
		if err := json.NewEncoder(w).Encode(claws); err != nil {
			t.Fatalf("encode claws: %v", err)
		}
	}))
	defer server.Close()

	out, err := captureStdout(func() error {
		return runListHub(&types.HubProfile{URL: server.URL, Token: "test-token"})
	})
	if err != nil {
		t.Fatalf("runListHub returned error: %v", err)
	}
	if !strings.Contains(out, "abc") {
		t.Fatalf("output missing short ID: %q", out)
	}
}

func TestRunFactoryTriggerHandlesShortClawID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/factories/demo/trigger" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"claw_id":"abc","status":"running"}`)
	}))
	defer server.Close()

	t.Setenv("ELASTICCLAW_HUB_URL", server.URL)
	t.Setenv("ELASTICCLAW_CLAW_TOKEN", "test-token")

	out, err := captureStdout(func() error {
		return runFactoryTrigger("demo", nil)
	})
	if err != nil {
		t.Fatalf("runFactoryTrigger returned error: %v", err)
	}
	if !strings.Contains(out, "claw abc") {
		t.Fatalf("output missing short claw ID: %q", out)
	}
}

func captureStdout(fn func() error) (string, error) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w

	fnErr := fn()
	w.Close()
	os.Stdout = old

	out, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		return "", readErr
	}
	return string(out), fnErr
}
