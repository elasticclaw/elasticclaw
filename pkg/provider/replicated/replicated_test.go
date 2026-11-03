package replicated

import (
	"context"
	"io"
	"net/http"
	"strings"
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

func TestVMTerminated(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body          string
		status              int
		terminated, wantErr bool
	}{
		{"not found", ``, 404, true, false},
		{"terminated", `{"vm":{"status":"terminated"}}`, 200, true, false},
		{"deleted", `{"vm":{"status":"deleted"}}`, 200, true, false},
		{"expired", `{"vm":{"status":"expired"}}`, 200, true, false},
		{"past expiry", `{"vm":{"status":"running","expires_at":"2026-11-02T11:59:59Z"}}`, 200, true, false},
		{"future expiry", `{"vm":{"status":"running","expires_at":"2026-11-04T12:00:00Z"}}`, 200, false, false},
		{"running", `{"vm":{"status":"running"}}`, 200, false, false},
		{"provisioning", `{"vm":{"status":"provisioning"}}`, 200, false, false},
		{"unauthorized", ``, 401, false, true},
		{"server error", `HTTP 404`, 500, false, true},
		{"missing VM", `{}`, 200, false, true},
		{"invalid response", `{`, 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Config{Token: "test-token"})
			if err != nil {
				t.Fatal(err)
			}
			p.http.Transport = vmLookupTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/vendor/v3/vm/vm-abc123" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			got, err := p.VMTerminated(context.Background(), "vm-abc123", at)
			if got != tc.terminated || (err != nil) != tc.wantErr {
				t.Fatalf("VMTerminated = %v, %v; want %v, error=%v", got, err, tc.terminated, tc.wantErr)
			}
		})
	}
}

type vmLookupTransport func(*http.Request) (*http.Response, error)

func (f vmLookupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
