//go:build integration

package hub_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
)

func buildExternalReleasePayload(repo, tag string) []byte {
	payload := map[string]interface{}{
		"action": "released",
		"release": map[string]interface{}{
			"tag_name":     tag,
			"name":         "Release " + tag,
			"body":         "Release notes",
			"html_url":     fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, tag),
			"prerelease":   false,
			"draft":        false,
			"created_at":   time.Now().UTC().Format(time.RFC3339),
			"published_at": time.Now().UTC().Format(time.RFC3339),
			"author": map[string]interface{}{
				"login": "testuser",
			},
		},
		"repository": map[string]interface{}{
			"full_name": repo,
			"name":      "testrepo",
			"owner": map[string]interface{}{
				"login": "testorg",
			},
			"html_url": fmt.Sprintf("https://github.com/%s", repo),
		},
		"sender": map[string]interface{}{
			"login": "testuser",
			"type":  "User",
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func signPayload(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func buildGenericWebhookPayload(repo string) []byte {
	payload := map[string]interface{}{
		"event_type": "generic-event",
		"repository": map[string]interface{}{
			"full_name": repo,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func sendGenericWebhook(t *testing.T, ts *factorytest.TestServer, body []byte, deliveryID string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL()+"/api/integrations/external/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signPayload(body, "test-webhook-secret"))
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func waitForExternalClawCount(t *testing.T, ts *factorytest.TestServer, factoryName string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE factory_name=?`, factoryName).Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
		if count == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var count int
	if err := ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE factory_name=?`, factoryName).Scan(&count); err != nil {
		t.Fatalf("final count query failed: %v", err)
	}
	t.Fatalf("expected %d claws for factory %q, got %d", want, factoryName, count)
}

func TestExternalWebhook_GenericEvent_DifferentDeliveryIDsBothCreateRuns(t *testing.T) {
	ts := factorytest.NewTestServerWithExternal(t)
	body := buildGenericWebhookPayload("testorg/testrepo")

	sendGenericWebhook(t, ts, body, "generic-delivery-1")
	sendGenericWebhook(t, ts, body, "generic-delivery-2")

	waitForExternalClawCount(t, ts, "generic-webhook-factory", 2)
}

func TestExternalWebhook_GenericEvent_SameDeliveryIDIsDeduped(t *testing.T) {
	ts := factorytest.NewTestServerWithExternal(t)
	body := buildGenericWebhookPayload("testorg/testrepo")

	sendGenericWebhook(t, ts, body, "generic-delivery-retry")
	waitForExternalClawCount(t, ts, "generic-webhook-factory", 1)
	sendGenericWebhook(t, ts, body, "generic-delivery-retry")

	time.Sleep(200 * time.Millisecond)
	var count int
	if err := ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE factory_name=?`, "generic-webhook-factory").Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 claw for retry delivery, got %d", count)
	}
}

func TestExternalWebhook_ConcurrentDeliveryCreatesOneClaw(t *testing.T) {
	ts := factorytest.NewTestServerWithExternal(t)

	repo := "testorg/testrepo"
	tag := "v1.0.0"
	triggerID := fmt.Sprintf("release-factory:%s@%s", repo, tag)
	body := buildExternalReleasePayload(repo, tag)
	sig := signPayload(body, "test-webhook-secret")

	// Fire 5 concurrent webhook requests for the same release
	var wg sync.WaitGroup
	statusCodes := make([]int, 5)
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req, err := http.NewRequest("POST", ts.URL()+"/api/integrations/external/webhook", bytes.NewReader(body))
			if err != nil {
				t.Errorf("request build failed: %v", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-GitHub-Event", "release")
			req.Header.Set("X-Hub-Signature-256", sig)
			// Use same delivery ID to test dedup + atomic claim together
			req.Header.Set("X-GitHub-Delivery", "delivery-123")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("request failed: %v", err)
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			statusCodes[idx] = resp.StatusCode
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	// All requests should return 200 (deduped or processed)
	for i, code := range statusCodes {
		if code != 200 {
			t.Fatalf("concurrent request %d: expected status 200, got %d", i, code)
		}
	}

	// Wait for at most one claw to be created
	clawID := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		ts.DB.QueryRow(`SELECT id FROM claws WHERE external_trigger_id=?`, triggerID).Scan(&id)
		if id != "" {
			clawID = id
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if clawID == "" {
		t.Fatal("expected at least one claw to be created")
	}

	// Verify exactly one claw exists for this trigger
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE external_trigger_id=?`, triggerID).Scan(&count)
	if err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 claw for trigger %s, got %d", triggerID, count)
	}

	// Verify factory_triggers has exactly one claim for this trigger
	var triggerCount int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM factory_triggers WHERE factory_name=? AND integration=? AND trigger_key=?`,
		"release-factory", "external", triggerID).Scan(&triggerCount)
	if err != nil {
		t.Fatalf("trigger count query failed: %v", err)
	}
	if triggerCount != 1 {
		t.Fatalf("expected exactly 1 factory_trigger row, got %d", triggerCount)
	}

	// Verify the trigger is linked to the claw
	var linkedClawID string
	err = ts.DB.QueryRow(`SELECT claw_id FROM factory_triggers WHERE factory_name=? AND integration=? AND trigger_key=?`,
		"release-factory", "external", triggerID).Scan(&linkedClawID)
	if err != nil {
		t.Fatalf("linked claw query failed: %v", err)
	}
	if linkedClawID != clawID {
		t.Fatalf("expected factory_trigger.claw_id=%s, got %s", clawID, linkedClawID)
	}
}

func TestExternalWebhook_SecondDeliveryIsIdempotent(t *testing.T) {
	ts := factorytest.NewTestServerWithExternal(t)

	repo := "testorg/testrepo"
	tag := "v2.0.0"
	triggerID := fmt.Sprintf("release-factory:%s@%s", repo, tag)
	body := buildExternalReleasePayload(repo, tag)
	sig := signPayload(body, "test-webhook-secret")

	// First delivery
	req1, _ := http.NewRequest("POST", ts.URL()+"/api/integrations/external/webhook", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-GitHub-Event", "release")
	req1.Header.Set("X-Hub-Signature-256", sig)
	req1.Header.Set("X-GitHub-Delivery", "delivery-first")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("first request: expected 200, got %d", resp1.StatusCode)
	}

	// Wait for claw to be created
	clawID := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		ts.DB.QueryRow(`SELECT id FROM claws WHERE external_trigger_id=?`, triggerID).Scan(&id)
		if id != "" {
			clawID = id
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clawID == "" {
		t.Fatal("expected claw to be created after first delivery")
	}

	// Second delivery (different delivery ID to bypass dedup, same trigger)
	req2, _ := http.NewRequest("POST", ts.URL()+"/api/integrations/external/webhook", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-GitHub-Event", "release")
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-GitHub-Delivery", "delivery-second")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("second request: expected 200, got %d", resp2.StatusCode)
	}

	// Give time for any potential second claw creation
	time.Sleep(500 * time.Millisecond)

	// Verify still exactly one claw
	var count int
	err = ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE external_trigger_id=?`, triggerID).Scan(&count)
	if err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 claw after second delivery, got %d", count)
	}
}
