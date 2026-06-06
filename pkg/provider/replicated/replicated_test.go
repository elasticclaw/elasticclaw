package replicated

import (
	"context"
	"testing"

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
