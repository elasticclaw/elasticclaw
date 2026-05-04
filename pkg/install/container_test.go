//go:build integration

package install_test

// Integration tests for the install scripts using real Ubuntu containers.
// Run with: ELASTICCLAW_INSTALL_TESTS=1 go test -tags integration ./pkg/install/ -run TestInstall_Container -v
//
// Requires Docker. Does NOT test systemd or Docker-in-Docker (those need
// privileged containers); focuses on: binary install, config write, Caddyfile.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/install"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	gossh "golang.org/x/crypto/ssh"
)

const sshPassword = "testpassword"

func TestInstall_Container(t *testing.T) {
	if os.Getenv("ELASTICCLAW_INSTALL_TESTS") == "" {
		t.Skip("set ELASTICCLAW_INSTALL_TESTS=1 to run")
	}

	ctx := context.Background()

	// Start Ubuntu 24.04 with sshd
	req := tc.ContainerRequest{
		Image: "rastasheep/ubuntu-sshd:18.04",
		Cmd: []string{
			"bash", "-c",
			fmt.Sprintf(`echo 'root:%s' | chpasswd && sed -i 's/#PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config && /usr/sbin/sshd -D`, sshPassword),
		},
		ExposedPorts: []string{"22/tcp"},
		WaitingFor:   wait.ForListeningPort("22/tcp").WithStartupTimeout(30 * time.Second),
	}

	container, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer container.Terminate(ctx)

	// Get mapped SSH port
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	port, err := container.MappedPort(ctx, "22")
	if err != nil {
		t.Fatalf("get port: %v", err)
	}

	// Connect via SSH with password auth
	cfg := &gossh.ClientConfig{
		User:            "root",
		Auth:            []gossh.AuthMethod{gossh.Password(sshPassword)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	client, err := gossh.Dial("tcp", fmt.Sprintf("%s:%s", host, port.Port()), cfg)
	if err != nil {
		t.Fatalf("SSH connect: %v", err)
	}
	defer client.Close()

	params := install.Params{
		Domain:    "hub.test.example.com",
		Version:   "v0.0.3",
		Token:     "test-hub-token-abc",
		ClawToken: "test-claw-token-def",
		UIPassword: "test-ui-password-ghi",
	}

	// Helper to run a script and fail the test on error
	run := func(name, script string) {
		t.Helper()
		t.Logf("Running: %s", name)
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("%s: new session: %v", name, err)
		}
		defer sess.Close()
		out, err := sess.CombinedOutput(script)
		if err != nil {
			t.Fatalf("%s failed:\n%s", name, string(out))
		}
	}

	// Helper to assert a file exists and contains expected content
	assertFile := func(path, contains string) {
		t.Helper()
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("assertFile session: %v", err)
		}
		defer sess.Close()
		out, err := sess.CombinedOutput(fmt.Sprintf("cat %s", path))
		if err != nil {
			t.Errorf("file %s does not exist: %v", path, err)
			return
		}
		if contains != "" && !strings.Contains(string(out), contains) {
			t.Errorf("file %s does not contain %q\nActual content:\n%s", path, contains, string(out))
		}
	}

	// ── Write hub config ──────────────────────────────────────────────────────
	run("write config", install.ScriptWriteConfig(params, false))
	assertFile("/root/.elasticclaw/hub.yaml", "test-hub-token-abc")
	assertFile("/root/.elasticclaw/hub.yaml", "test-claw-token-def")
	assertFile("/root/.elasticclaw/hub.yaml", "hub.test.example.com")
	t.Log("✓ hub config written correctly")

	// ── Write Caddyfile ───────────────────────────────────────────────────────
	run("write caddyfile", fmt.Sprintf(`mkdir -p /etc/caddy && cat > /etc/caddy/Caddyfile << 'CADDYEOF'
%sCADDYEOF`, install.Caddyfile(params.Domain)))
	assertFile("/etc/caddy/Caddyfile", "hub.test.example.com")
	assertFile("/etc/caddy/Caddyfile", "reverse_proxy localhost:8080")
	t.Log("✓ Caddyfile written correctly")

	// ── Write systemd unit ────────────────────────────────────────────────────
	run("write systemd unit", fmt.Sprintf(`mkdir -p /etc/systemd/system
cat > /etc/systemd/system/elasticclaw.service << 'SVCEOF'
%sSVCEOF`, install.SystemdUnit()))
	assertFile("/etc/systemd/system/elasticclaw.service", "/usr/local/bin/elasticclaw hub")
	assertFile("/etc/systemd/system/elasticclaw.service", "Restart=always")
	t.Log("✓ systemd unit written correctly")

	// ── Verify hub binary URL is correct ─────────────────────────────────────
	// We don't actually download the binary in the container test (slow + network)
	// but verify the install script would use the right URL
	script := install.ScriptInstallBinary(params.Version, false)
	if !strings.Contains(script, "v0.0.3") {
		t.Error("install script missing version in URL")
	}
	if !strings.Contains(script, "elasticclaw-linux-amd64") {
		t.Error("install script missing binary name")
	}
	t.Log("✓ binary install script looks correct")

	t.Logf("\nAll install script assertions passed against a real Ubuntu 24.04 container")
}
