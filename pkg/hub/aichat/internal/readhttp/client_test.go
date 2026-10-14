package readhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestClientRedactsJSONAndPreservesNumbers(t *testing.T) {
	const secret = "secret-with-\"quote"
	c := Client{BaseURL: "https://provider.test", Secrets: []string{secret}, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		_ = json.NewEncoder(rec).Encode(map[string]any{"secret": secret, "id": int64(9007199254740993)})
		return rec.Result(), nil
	})}}
	var result struct {
		Secret string
		ID     int64
	}
	if err := c.Do(context.Background(), "GET", "/read", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result.Secret != "[redacted]" || result.ID != 9007199254740993 {
		t.Fatalf("result = %+v", result)
	}
	raw, _ := json.Marshal(map[string]string{"key": secret})
	if got := RedactJSON(string(raw), []string{secret}); got != `{"key":"[redacted]"}` {
		t.Fatal(got)
	}
}
func TestClientRejectsRedirectsAndProviderErrors(t *testing.T) {
	for _, status := range []int{302, 401, 500} {
		calls := 0
		c := Client{BaseURL: "https://provider.test", Headers: http.Header{"Authorization": {"sentinel"}}, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			calls++
			rec := httptest.NewRecorder()
			rec.Header().Set("Location", "https://other.test/stolen")
			rec.WriteHeader(status)
			fmt.Fprint(rec, "sentinel")
			return rec.Result(), nil
		})}}
		err := c.Do(context.Background(), "GET", "/read", nil, nil)
		if err == nil || strings.Contains(err.Error(), "sentinel") || calls != 1 {
			t.Fatalf("status %d: %v (%d calls)", status, err, calls)
		}
	}
}

func TestMapResultsKeepLargeIntegers(t *testing.T) {
	c := Client{BaseURL: "https://provider.test", HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		fmt.Fprint(rec, `{"id":9007199254740993}`)
		return rec.Result(), nil
	})}}
	var out map[string]any
	if err := c.Do(context.Background(), "GET", "/read", nil, &out); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(out)
	if string(encoded) != `{"id":9007199254740993}` {
		t.Fatal(string(encoded))
	}
	if got := RedactJSON(`{} {}`, nil); got != "null" {
		t.Fatalf("malformed JSON became %s", got)
	}
}

func TestStreamRedactorHandlesEverySplit(t *testing.T) {
	const secret = "sentinel-secret"
	for split := 0; split <= len(secret); split++ {
		redactor := StreamRedactor{Secrets: []string{secret, "sentinel-secret-longer"}}
		output := redactor.Write("before "+secret[:split]) + redactor.Write(secret[split:]+" after") + redactor.Flush()
		if output != "before [redacted] after" {
			t.Fatalf("split %d: %q", split, output)
		}
	}
}
