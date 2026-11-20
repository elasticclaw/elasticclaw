package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPProxyLogsOmitSecrets(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)

	var proxy *httpProxy
	proxy = newHTTPProxy(func(msg hubMsg) error {
		var req httpProxyReq
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			return err
		}
		if req.Query != "token=query-secret" || req.Header["Authorization"] != "Bearer header-secret" {
			t.Errorf("forwarded request changed: %+v", req)
		}
		proxy.deliver(httpProxyRes{ReqID: req.ReqID, Status: http.StatusForbidden})
		return nil
	})
	req := httptest.NewRequest("GET", "/private-path?token=query-secret", nil)
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("Cookie", "session=cookie-secret")
	req.Header.Set("X-Test", "ordinary-header-secret")
	res := httptest.NewRecorder()
	proxy.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d", res.Code)
	}
	logs := output.String()
	for _, secret := range []string{"query-secret", "header-secret", "cookie-secret", "ordinary-header-secret", "Authorization", "Cookie", "token="} {
		if strings.Contains(logs, secret) {
			t.Errorf("proxy log contains %q: %s", secret, logs)
		}
	}
	for _, expected := range []string{"GET", "/private-path", "status=403"} {
		if !strings.Contains(logs, expected) {
			t.Errorf("proxy log missing %q: %s", expected, logs)
		}
	}
}
