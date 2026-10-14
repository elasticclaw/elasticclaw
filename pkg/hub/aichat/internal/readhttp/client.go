// Package readhttp bounds provider responses and prevents credential-bearing errors.
package readhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const MaxResponseBytes = 4 << 20

type Client struct {
	HTTP    *http.Client
	BaseURL string
	Headers http.Header
	Secrets []string
}

type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("Provider request failed (HTTP %d).", e.Code)
}

func (c Client) Do(ctx context.Context, method, path string, body, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if method != http.MethodGet && method != http.MethodPost {
		return errors.New("unsupported read method")
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("invalid provider path")
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("invalid provider request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid provider endpoint")
	}
	req.Header = c.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.DefaultClient
	if c.HTTP != nil {
		client = c.HTTP
	}
	// A redirect must never forward credentials or turn a query into another request.
	safe := *client
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := safe.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Unable to reach the provider.")
	}
	defer res.Body.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &StatusError{Code: res.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return errors.New("Unable to read the provider response.")
	}
	if out == nil {
		return nil
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return errors.New("Invalid provider response.")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("Invalid provider response.")
	}
	scrubbed, _ := json.Marshal(scrub(decoded, c.Secrets))
	resultDecoder := json.NewDecoder(bytes.NewReader(scrubbed))
	resultDecoder.UseNumber()
	if resultDecoder.Decode(out) != nil {
		return errors.New("Invalid provider response.")
	}
	return nil
}

func Redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}
func scrub(value any, secrets []string) any {
	switch v := value.(type) {
	case string:
		return Redact(v, secrets)
	case []any:
		for i, item := range v {
			v[i] = scrub(item, secrets)
		}
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			result[Redact(key, secrets)] = scrub(item, secrets)
		}
		return result
	}
	return value
}

// RedactJSON preserves valid JSON even when a secret contains quotes or escapes.
func RedactJSON(raw string, secrets []string) string {
	var v any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return `null`
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return `null`
	}
	data, _ := json.Marshal(scrub(v, secrets))
	return string(data)
}
