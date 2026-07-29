package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	defaultSlackBaseURL = "https://slack.com/api"
	// slackMaxAttempts caps 429 retries within a single postMessage call.
	slackMaxAttempts = 4
	// slackMinSendInterval paces chat.postMessage: Slack allows roughly one
	// message per second per channel.
	slackMinSendInterval = time.Second
)

// slackAPIError is a logical Slack API failure (HTTP 200 with ok:false).
// Code carries the Slack error string, e.g. "channel_not_found".
type slackAPIError struct {
	Code string
}

func (e *slackAPIError) Error() string {
	return "slack API error: " + e.Code
}

// isSlackConfigError reports whether a Slack failure is a configuration-level
// problem (bad token, missing channel) that fails every message alike. The
// notifier must pause on these rather than record each event as failed:
// burning the stream on a routine token rotation would permanently lose every
// notification generated before the operator fixes the config.
func isSlackConfigError(err error) bool {
	var apiErr *slackAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "invalid_auth", "account_inactive", "token_revoked",
		"channel_not_found", "not_in_channel", "is_archived":
		return true
	}
	return false
}

// isPermanentSlackError reports whether a Slack failure is specific to this
// message and will never succeed on retry (oversized or malformed payload),
// so the delivery should be recorded as failed instead of retried forever.
func isPermanentSlackError(err error) bool {
	var apiErr *slackAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "msg_too_long", "invalid_blocks", "invalid_blocks_format":
		return true
	}
	return false
}

// slackSendLimiter serializes sends and enforces a minimum interval between
// them so bursts of parallel agents cannot hammer chat.postMessage.
type slackSendLimiter struct {
	mu       sync.Mutex
	lastSend time.Time
}

// wait blocks until at least minInterval has passed since the previous send,
// then claims the send slot. Sends are serialized by the mutex. A cancelled or
// expired context aborts the wait immediately (releasing the mutex) so callers
// with a deadline are not pinned behind a long send queue.
func (l *slackSendLimiter) wait(ctx context.Context, minInterval time.Duration) error {
	if l == nil || minInterval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if wait := minInterval - time.Since(l.lastSend); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	l.lastSend = time.Now()
	return nil
}

// slackClient is a minimal chat.postMessage client over net/http.
type slackClient struct {
	token      string
	httpClient *http.Client
	// baseURL defaults to https://slack.com/api; tests point it at an httptest.Server.
	baseURL string
	// limiter paces sends across all callers sharing it (may be nil in tests).
	limiter *slackSendLimiter
	// minSendInterval overrides the 1s pacing (tests set it near zero).
	minSendInterval time.Duration
}

func (c *slackClient) apiBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return defaultSlackBaseURL
}

// postMessage posts a Block Kit message to a channel (optionally into a
// thread) and returns the message ts. fallback is the required plain-text
// notification fallback. Slack signals logical errors as HTTP 200 with
// ok:false, which surfaces as *slackAPIError. HTTP 429 is retried honoring
// Retry-After, up to slackMaxAttempts total attempts.
func (c *slackClient) postMessage(ctx context.Context, channel, threadTS string, blocks []any, fallback string) (string, error) {
	payload := map[string]any{
		"channel": channel,
		"text":    fallback,
	}
	if len(blocks) > 0 {
		payload["blocks"] = blocks
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal slack message: %w", err)
	}

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	interval := c.minSendInterval
	if interval == 0 {
		interval = slackMinSendInterval
	}

	for attempt := 1; ; attempt++ {
		if err := c.limiter.wait(ctx, interval); err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/chat.postMessage", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= slackMaxAttempts {
				return "", fmt.Errorf("slack rate limited after %d attempts", attempt)
			}
			delay := slackRetryAfter(resp.Header.Get("Retry-After"), attempt)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("slack HTTP %d: %s", resp.StatusCode, truncateForLog(string(respBody), 300))
		}

		var result struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			TS    string `json:"ts"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", fmt.Errorf("parse slack response: %w", err)
		}
		if !result.OK {
			return "", &slackAPIError{Code: result.Error}
		}
		return result.TS, nil
	}
}

// slackRetryAfter parses a Retry-After header (seconds); when absent or
// malformed it falls back to a small exponential backoff.
func slackRetryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Duration(attempt) * time.Second
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
