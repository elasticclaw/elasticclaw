package replicated

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// TestCreateRejectsTemplateFiles verifies that Create returns an error when
// TemplateFiles are provided, since Replicated CMX VMs cannot inject files
// at creation time (SSH is not yet available).
func TestCreateRejectsTemplateFiles(t *testing.T) {
	p := &Provider{
		apiURL:      DefaultAPIURL,
		token:       "test-token",
		defaultTTL:  DefaultTTL,
		defaultType: DefaultInstanceType,
	}

	req := types.CreateRequest{
		Name: "test-claw",
		TemplateFiles: map[string][]byte{
			"hello.txt": []byte("hello world"),
		},
	}

	_, err := p.Create(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for TemplateFiles, got nil")
	}
	want := "replicated provider does not support TemplateFiles in Create: files must be injected after VM is running via SSH"
	if err.Error() != want {
		t.Errorf("error message mismatch\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestCreateRejectsEnv verifies that Create returns an error when Env vars are
// provided, since Replicated CMX VMs cannot set environment variables at
// creation time (SSH is not yet available).
func TestCreateRejectsEnv(t *testing.T) {
	p := &Provider{
		apiURL:      DefaultAPIURL,
		token:       "test-token",
		defaultTTL:  DefaultTTL,
		defaultType: DefaultInstanceType,
	}

	req := types.CreateRequest{
		Name: "test-claw",
		Env: map[string]string{
			"FOO": "bar",
		},
	}

	_, err := p.Create(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for Env, got nil")
	}
	want := "replicated provider does not support Env in Create: environment variables must be set after VM is running via SSH"
	if err.Error() != want {
		t.Errorf("error message mismatch\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestCreateRejectsBothTemplateFilesAndEnv verifies that Create returns an
// error when both TemplateFiles and Env are provided.
func TestCreateRejectsBothTemplateFilesAndEnv(t *testing.T) {
	p := &Provider{
		apiURL:      DefaultAPIURL,
		token:       "test-token",
		defaultTTL:  DefaultTTL,
		defaultType: DefaultInstanceType,
	}

	req := types.CreateRequest{
		Name: "test-claw",
		TemplateFiles: map[string][]byte{
			"hello.txt": []byte("hello world"),
		},
		Env: map[string]string{
			"FOO": "bar",
		},
	}

	_, err := p.Create(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// TemplateFiles check comes first
	want := "replicated provider does not support TemplateFiles in Create: files must be injected after VM is running via SSH"
	if err.Error() != want {
		t.Errorf("error message mismatch\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestProvisionClawRejectsTemplateFiles verifies that ProvisionClaw returns an
// error when templateFiles are provided, since the Replicated CMX API does
// not support file injection at creation time.
func TestProvisionClawRejectsTemplateFiles(t *testing.T) {
	p := &Provider{
		apiURL:      DefaultAPIURL,
		token:       "test-token",
		defaultTTL:  DefaultTTL,
		defaultType: DefaultInstanceType,
	}

	_, err := p.ProvisionClaw(
		context.Background(),
		VMCreateRequest{Name: "test-claw"},
		map[string][]byte{"hello.txt": []byte("hello world")},
		nil,
	)
	if err == nil {
		t.Fatal("expected error for templateFiles, got nil")
	}
	want := "replicated provider does not support templateFiles in ProvisionClaw: files must be injected after VM is running via SSH"
	if err.Error() != want {
		t.Errorf("error message mismatch\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestProvisionClawRejectsEnv verifies that ProvisionClaw returns an error
// when env vars are provided, since the Replicated CMX API does not support
// environment variable injection at creation time.
func TestProvisionClawRejectsEnv(t *testing.T) {
	p := &Provider{
		apiURL:      DefaultAPIURL,
		token:       "test-token",
		defaultTTL:  DefaultTTL,
		defaultType: DefaultInstanceType,
	}

	_, err := p.ProvisionClaw(
		context.Background(),
		VMCreateRequest{Name: "test-claw"},
		nil,
		map[string]string{"FOO": "bar"},
	)
	if err == nil {
		t.Fatal("expected error for env, got nil")
	}
	want := "replicated provider does not support env in ProvisionClaw: environment variables must be set after VM is running via SSH"
	if err.Error() != want {
		t.Errorf("error message mismatch\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestProvisionClawRejectsBothTemplateFilesAndEnv verifies that ProvisionClaw
// returns an error when both templateFiles and env are provided.
func TestProvisionClawRejectsBothTemplateFilesAndEnv(t *testing.T) {
	p := &Provider{
		apiURL:      DefaultAPIURL,
		token:       "test-token",
		defaultTTL:  DefaultTTL,
		defaultType: DefaultInstanceType,
	}

	_, err := p.ProvisionClaw(
		context.Background(),
		VMCreateRequest{Name: "test-claw"},
		map[string][]byte{"hello.txt": []byte("hello world")},
		map[string]string{"FOO": "bar"},
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// templateFiles check comes first
	want := "replicated provider does not support templateFiles in ProvisionClaw: files must be injected after VM is running via SSH"
	if err.Error() != want {
		t.Errorf("error message mismatch\ngot:  %s\nwant: %s", err.Error(), want)
	}
}

// TestSSHUserFromPublicKey verifies SSH username extraction from public key comments.
func TestSSHUserFromPublicKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "ed25519 with email comment",
			key:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID test@example.com",
			want: "test",
		},
		{
			name: "ed25519 with plain comment",
			key:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID elasticclaw@hub",
			want: "elasticclaw",
		},
		{
			name: "no comment",
			key:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID",
			want: "ubuntu",
		},
		{
			name: "empty string",
			key:  "",
			want: "ubuntu",
		},
		{
			name: "comment without @",
			key:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID myuser",
			want: "myuser",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SSHUserFromPublicKey(tt.key)
			if got != tt.want {
				t.Errorf("SSHUserFromPublicKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestCreateVMRetries(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		body        string
		failures    int
		wantPosts   int32
		wantLists   int32
		wantError   bool
		wantCredits bool
	}{
		{"502 then success", 502, "bad gateway", 1, 2, 1, false, false},
		{"bounded retries", 503, "unavailable", 3, 3, 3, true, false},
		{"credits", 400, "Request exceeds available credits", 1, 1, 0, true, true},
		{"other bad request", 400, "invalid name", 1, 1, 0, true, false},
		{"unauthorized", 401, "unauthorized", 1, 1, 0, true, false},
		{"rate limited", 429, "too many requests", 1, 1, 0, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var posts, lists atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/vendor/v3/vm":
					if int(posts.Add(1)) <= tt.failures {
						http.Error(w, tt.body, tt.status)
						return
					}
					w.WriteHeader(http.StatusCreated)
					w.Write([]byte(`{"vms":[{"id":"vm-created","name":"test-claw"}]}`))
				case r.Method == http.MethodGet && r.URL.Path == "/vendor/v3/vms":
					lists.Add(1)
					w.Write([]byte(`{"vms":[]}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			p, err := New(Config{Token: "test-token", APIURL: server.URL + "/vendor/v3"})
			if err != nil {
				t.Fatal(err)
			}
			p.createBackoff = [2]time.Duration{}
			id, err := p.CreateVM(context.Background(), VMCreateRequest{Name: "test-claw"})
			if (err != nil) != tt.wantError || errors.Is(err, ErrInsufficientCredits) != tt.wantCredits {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantError && id != "vm-created" {
				t.Errorf("VM ID = %q", id)
			}
			if posts.Load() != tt.wantPosts || lists.Load() != tt.wantLists {
				t.Errorf("POSTs = %d, lists = %d, want %d, %d", posts.Load(), lists.Load(), tt.wantPosts, tt.wantLists)
			}
		})
	}
}

func TestCreateVMReconcilesGatewayError(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var posts, lists atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/vm":
					// CMX created the VM, but the gateway returned an error.
					posts.Add(1)
					http.Error(w, "gateway error", status)
				case r.Method == http.MethodGet && r.URL.Path == "/vms":
					lists.Add(1)
					w.Write([]byte(`{"vms":[{"id":"vm-created","name":"test-claw","status":"queued"}]}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			p, err := New(Config{Token: "test-token", APIURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			p.createBackoff = [2]time.Duration{}
			id, err := p.CreateVM(context.Background(), VMCreateRequest{Name: "test-claw"})
			if err != nil || id != "vm-created" {
				t.Fatalf("CreateVM = %q, %v", id, err)
			}
			if posts.Load() != 1 || lists.Load() != 1 {
				t.Errorf("POSTs = %d, lists = %d", posts.Load(), lists.Load())
			}
		})
	}
}

func TestCreateVMReconcilesTimeout(t *testing.T) {
	for _, tt := range []struct {
		name       string
		listBody   string
		listStatus int
		wantPosts  int32
		wantLists  int32
		wantError  bool
	}{
		{"existing VM", `{"vms":[{"id":"other","name":"other"},{"id":"vm-created","name":"test-claw"}]}`, 200, 1, 1, false},
		{"missing VM", `{"vms":[]}`, 200, 2, 1, false},
		{"list failed", "unavailable", 503, 1, 3, true},
		{"invalid list", "{", 200, 1, 3, true},
		{"skips terminated predecessor", `{"vms":[{"id":"old","name":"test-claw","status":"terminated","created_at":"2026-11-02T10:00:00Z"}]}`, 200, 2, 1, false},
		{"skips VM created before the request", `{"vms":[{"id":"unrelated","name":"test-claw","status":"running","created_at":"2026-11-02T09:00:00Z"}]}`, 200, 2, 1, false},
		{"prefers newest live VM", `{"vms":[{"id":"older","name":"test-claw","status":"running","created_at":"2026-11-02T10:58:00Z"},{"id":"vm-created","name":"test-claw","status":"queued","created_at":"2026-11-02T11:00:00Z"},{"id":"expired","name":"test-claw","status":"expired","created_at":"2026-11-02T12:00:00Z"}]}`, 200, 1, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var posts, lists atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/vm":
					if posts.Add(1) == 1 {
						io.Copy(io.Discard, r.Body)
						<-r.Context().Done()
						return
					}
					w.WriteHeader(http.StatusCreated)
					w.Write([]byte(`{"vms":[{"id":"vm-created","name":"test-claw"}]}`))
				case r.Method == http.MethodGet && r.URL.Path == "/vms":
					lists.Add(1)
					w.WriteHeader(tt.listStatus)
					w.Write([]byte(tt.listBody))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			p, err := New(Config{Token: "test-token", APIURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			p.createBackoff = [2]time.Duration{}
			p.now = func() time.Time { return time.Date(2026, 11, 2, 11, 2, 0, 0, time.UTC) }
			p.http.Timeout = 100 * time.Millisecond
			id, err := p.CreateVM(context.Background(), VMCreateRequest{Name: "test-claw"})
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantError && id != "vm-created" {
				t.Errorf("VM ID = %q", id)
			}
			if posts.Load() != tt.wantPosts || lists.Load() != tt.wantLists {
				t.Errorf("POSTs = %d, lists = %d", posts.Load(), lists.Load())
			}
		})
	}
}

func TestCreateVMRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	p, err := New(Config{Token: "test-token", APIURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	p.createBackoff = [2]time.Duration{time.Hour, time.Hour}
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = p.CreateVM(ctx, VMCreateRequest{Name: "test-claw"})
	if !errors.Is(err, context.Canceled) || posts.Load() != 1 {
		t.Fatalf("error = %v, POSTs = %d", err, posts.Load())
	}
}

func TestCreateVMReconcilesTimeoutOnLastAttempt(t *testing.T) {
	var posts, lists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/vm":
			if posts.Add(1) <= 2 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			// The third POST creates the VM but its response is lost.
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		case r.Method == http.MethodGet && r.URL.Path == "/vms":
			lists.Add(1)
			if posts.Load() < 3 {
				w.Write([]byte(`{"vms":[]}`))
				return
			}
			w.Write([]byte(`{"vms":[{"id":"vm-created","name":"test-claw","status":"queued"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p, err := New(Config{Token: "test-token", APIURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	p.createBackoff = [2]time.Duration{}
	p.http.Timeout = 100 * time.Millisecond
	id, err := p.CreateVM(context.Background(), VMCreateRequest{Name: "test-claw"})
	if err != nil || id != "vm-created" {
		t.Fatalf("CreateVM = %q, %v", id, err)
	}
	if posts.Load() != 3 || lists.Load() != 3 {
		t.Errorf("POSTs = %d, lists = %d", posts.Load(), lists.Load())
	}
}

func TestCreateVMRetriesTransientReconcileFailure(t *testing.T) {
	var posts, lists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/vm":
			// The VM is created, but the response is lost.
			posts.Add(1)
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		case r.Method == http.MethodGet && r.URL.Path == "/vms":
			if lists.Add(1) == 1 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"vms":[{"id":"vm-created","name":"test-claw","status":"queued"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p, err := New(Config{Token: "test-token", APIURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	p.createBackoff = [2]time.Duration{}
	p.http.Timeout = 100 * time.Millisecond
	id, err := p.CreateVM(context.Background(), VMCreateRequest{Name: "test-claw"})
	if err != nil || id != "vm-created" {
		t.Fatalf("CreateVM = %q, %v", id, err)
	}
	if posts.Load() != 1 || lists.Load() != 2 {
		t.Errorf("POSTs = %d, lists = %d", posts.Load(), lists.Load())
	}
}
