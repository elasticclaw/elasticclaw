package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestBridgeProxyWS(t *testing.T) {
	const clawID = "claw-proxy-bound"
	for _, tc := range []struct {
		name    string
		path    string
		routes  []bridgeProxyRoute
		headers map[string]string
		status  int
	}{
		{name: "default deny", path: "/api/proxy-test", status: http.StatusForbidden},
		{name: "no injected token", path: "/api/proxy-test", routes: []bridgeProxyRoute{{method: "GET", path: "/api/proxy-test"}}, status: http.StatusOK},
		{name: "strip credentials", path: "/api/proxy-test", routes: []bridgeProxyRoute{{method: "GET", path: "/api/proxy-test"}}, status: http.StatusOK,
			headers: map[string]string{
				"aUtHoRiZaTiOn": "Bearer private", "x-cLaW-ToKeN": "private-claw", "cOoKiE": "session=private",
				"X-Volume-Token": "private-volume", "x-FoRwArDeD-FoR": "private-ip", "X-Forwarded-Custom": "private-custom", "X-Test": "keep",
			}},
		{name: "claw mismatch", path: "/api/proxy-test/other-claw", routes: []bridgeProxyRoute{{method: "GET", path: "/api/proxy-test/", prefix: true, clawID: proxyTestClawID}}, status: http.StatusForbidden},
		{name: "claw matches", path: "/api/proxy-test/" + clawID, routes: []bridgeProxyRoute{{method: "GET", path: "/api/proxy-test/", prefix: true, clawID: proxyTestClawID}}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := bridgeProxyAllowedRoutes
			bridgeProxyAllowedRoutes = tc.routes
			t.Cleanup(func() { bridgeProxyAllowedRoutes = previous })
			s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,created_at) VALUES(?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "starting", now()); err != nil {
				t.Fatal(err)
			}
			var called atomic.Bool
			headers := make(chan http.Header, 1)
			handler := func(w http.ResponseWriter, r *http.Request) {
				called.Store(true)
				headers <- r.Header.Clone()
				w.WriteHeader(http.StatusOK)
			}
			s.mux.HandleFunc("/api/proxy-test", handler)
			s.mux.HandleFunc("/api/proxy-test/", handler)
			ts := httptest.NewServer(s.Handler())
			t.Cleanup(ts.Close)
			conn := connectTestClaw(t, ts, clawID)
			t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "http_proxy_req", Payload: map[string]interface{}{
				"req_id": "proxy-test", "method": "GET", "path": tc.path, "query": "secret=private", "header": tc.headers,
			}}); err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Type    string `json:"type"`
				Payload struct {
					ReqID  string `json:"req_id"`
					Status int    `json:"status"`
					Body   string `json:"body"`
				} `json:"payload"`
			}
			for reply.Type != "http_proxy_res" {
				if err := wsjson.Read(ctx, conn, &reply); err != nil {
					t.Fatal(err)
				}
			}
			if reply.Payload.ReqID != "proxy-test" || reply.Payload.Status != tc.status {
				t.Fatalf("unexpected reply: %+v", reply)
			}
			if called.Load() != (tc.status == http.StatusOK) {
				t.Fatalf("mux called = %v for status %d", called.Load(), tc.status)
			}
			if tc.status == http.StatusForbidden {
				var body map[string]string
				if err := json.Unmarshal([]byte(reply.Payload.Body), &body); err != nil || body["error"] == "" {
					t.Fatalf("expected JSON error, got %q", reply.Payload.Body)
				}
				return
			}
			got := <-headers
			for _, name := range []string{"Authorization", "X-Claw-Token", "Cookie", "X-Volume-Token", "X-Forwarded-For", "X-Forwarded-Custom"} {
				if len(got.Values(name)) != 0 {
					t.Errorf("sensitive header forwarded: %s", name)
				}
			}
			if got.Get("X-Test") != tc.headers["X-Test"] {
				t.Errorf("ordinary header lost: %v", got)
			}
		})
	}
}

func proxyTestClawID(path string) string {
	id := strings.TrimPrefix(path, "/api/proxy-test/")
	if strings.Contains(id, "/") {
		return ""
	}
	return id
}

func TestBridgeProxyRequestAllowed(t *testing.T) {
	previous := bridgeProxyAllowedRoutes
	defer func() { bridgeProxyAllowedRoutes = previous }()
	bridgeProxyAllowedRoutes = []bridgeProxyRoute{
		{method: "GET", path: "/exact"},
		{method: "POST", path: "/api/proxy-test/", prefix: true, clawID: proxyTestClawID},
	}
	for _, tc := range []struct {
		method, path, clawID string
		want                 bool
	}{
		{"GET", "/exact", "bound", true},
		{"POST", "/exact", "bound", false},
		{"GET", "/exact/extra", "bound", false},
		{"POST", "/api/proxy-test/bound", "bound", true},
		{"POST", "/api/proxy-test/other", "bound", false},
		{"POST", "/api/proxy-test/", "", false},
		{"POST", "/api/proxy-test/bound/extra", "bound", false},
		{"POST", "/api/proxy-test-other/bound", "bound", false},
	} {
		if got := bridgeProxyRequestAllowed(tc.method, tc.path, tc.clawID); got != tc.want {
			t.Errorf("allowed(%q, %q, %q) = %v, want %v", tc.method, tc.path, tc.clawID, got, tc.want)
		}
	}
}

func TestBridgeProxySensitiveHeader(t *testing.T) {
	for _, name := range []string{"Authorization", "X-Claw-Token", "Cookie", "X-Volume-Token", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Custom"} {
		for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			if !bridgeProxySensitiveHeader(variant) {
				t.Errorf("sensitive header allowed: %s", variant)
			}
		}
	}
	for _, name := range []string{"Content-Type", "Accept", "X-Test"} {
		if bridgeProxySensitiveHeader(name) {
			t.Errorf("ordinary header blocked: %s", name)
		}
	}
}
