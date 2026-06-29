package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
)

func TestCopyInRejectsRelativeDestination(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	err = provider.CopyIn(context.Background(), "container", "relative/path.txt", []byte("content"))
	if err == nil {
		t.Fatal("expected relative destination to be rejected")
	}
}

func TestCopyInPreparesDestinationAsRootAndRestoresOwnership(t *testing.T) {
	oldCommandContext := dockerCommandContext
	t.Cleanup(func() { dockerCommandContext = oldCommandContext })

	logPath := t.TempDir() + "/docker-commands.log"
	dockerCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmdArgs := append([]string{"-test.run=TestDockerCommandHelper", "--", name}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cmdArgs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_DOCKER_COMMAND_HELPER=1",
			"DOCKER_COMMAND_LOG="+logPath,
		)
		return cmd
	}

	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if err := provider.CopyIn(context.Background(), "container", "/home/claw/workspace/MEMORY.md", []byte("content")); err != nil {
		t.Fatalf("copy in: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read command log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"docker exec container id -u",
		"docker exec container id -g",
		"docker exec -u 0 container mkdir -p /home/claw/workspace",
		"docker exec -u 0 container chown 1001:1002 /home/claw/workspace",
		"docker cp - container:/home/claw/workspace",
		"docker exec -u 0 container chown 1001:1002 /home/claw/workspace/MEMORY.md",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("docker commands:\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDockerCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DOCKER_COMMAND_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}
	logPath := os.Getenv("DOCKER_COMMAND_LOG")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	_, _ = f.WriteString(strings.Join(args, " ") + "\n")
	_ = f.Close()

	command := strings.Join(args, " ")
	switch command {
	case "docker exec container id -u":
		_, _ = os.Stdout.WriteString("1001\n")
	case "docker exec container id -g":
		_, _ = os.Stdout.WriteString("1002\n")
	}
	os.Exit(0)
}

func TestNewDefaultsToPinnedOpenClawImage(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	if got, want := provider.cfg.Image, "ghcr.io/openclaw/openclaw:"+cliversion.OpenClawVersion; got != want {
		t.Fatalf("default image = %q, want %q", got, want)
	}
}
