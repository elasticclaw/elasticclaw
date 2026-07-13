package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const defaultSlackAPIBase = "https://slack.com/api"

// maxRetryAfter caps how long a 429 Retry-After can stall a pipeline action.
const maxRetryAfter = 30 * time.Second

type slackNotifier struct {
	token   string
	channel string
	apiBase string
	client  *http.Client
}

func newSlack(cfg map[string]any, secrets SecretResolver) (Notifier, error) {
	secretName := stringOption(cfg, "token_secret")
	if secretName == "" {
		return nil, fmt.Errorf("slack notifier requires token_secret")
	}
	token, ok := secrets(secretName)
	if !ok || token == "" {
		return nil, fmt.Errorf("slack notifier secret %q not found in hub secrets", secretName)
	}
	apiBase := stringOption(cfg, "api_base")
	if apiBase == "" {
		apiBase = defaultSlackAPIBase
	}
	return &slackNotifier{
		token:   token,
		channel: stringOption(cfg, "channel"),
		apiBase: apiBase,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (s *slackNotifier) Send(ctx context.Context, msg Message) error {
	channel := msg.Target
	if channel == "" {
		channel = s.channel
	}
	if channel == "" {
		return fmt.Errorf("slack notifier has no channel configured and the message sets no target")
	}

	payload := make(map[string]any, len(msg.Options)+2)
	for k, v := range msg.Options {
		payload[k] = v
	}
	payload["channel"] = channel
	payload["text"] = msg.Text

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}

	resp, err := s.post(ctx, body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		delay := retryAfter(resp)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		resp, err = s.post(ctx, body)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack API returned HTTP %d", resp.StatusCode)
	}

	var apiResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decode slack response: %w", err)
	}
	if !apiResp.OK {
		return fmt.Errorf("slack API error: %s", apiResp.Error)
	}
	return nil
}

func (s *slackNotifier) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack API request failed: %w", err)
	}
	return resp, nil
}

func retryAfter(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds < 0 {
		return time.Second
	}
	delay := time.Duration(seconds) * time.Second
	if delay > maxRetryAfter {
		return maxRetryAfter
	}
	return delay
}
