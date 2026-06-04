package hub

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetryReplicatedBootstrapStepSucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0
	var slept []time.Duration

	err := retryReplicatedBootstrapStep(nil, "", replicatedBootstrapRetryOptions{
		Label:      "Writing workspace files",
		RetryLabel: "Retrying workspace file write",
		Attempts:   4,
		Delays:     []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second},
		Sleep: func(d time.Duration) {
			slept = append(slept, d)
		},
		Run: func() error {
			attempts++
			if attempts < 3 {
				return errors.New("ssh dial: connect: connection refused")
			}
			return nil
		},
	})

	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if len(slept) != 2 || slept[0] != 5*time.Second || slept[1] != 10*time.Second {
		t.Fatalf("unexpected retry delays: %#v", slept)
	}
}

func TestRetryReplicatedBootstrapStepReturnsSanitizedFinalError(t *testing.T) {
	attempts := 0

	err := retryReplicatedBootstrapStep(nil, "", replicatedBootstrapRetryOptions{
		Label:      "Configuring GitHub credentials",
		RetryLabel: "Retrying GitHub credential setup",
		Attempts:   3,
		Delays:     []time.Duration{5 * time.Second, 10 * time.Second},
		Sleep:      func(time.Duration) {},
		Run: func() error {
			attempts++
			return errors.New(strings.Repeat("ssh dial: connect: connection refused\n", 100))
		},
	})

	if err == nil {
		t.Fatal("expected final error")
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if !strings.Contains(err.Error(), "Configuring GitHub credentials failed after 3 attempts") {
		t.Fatalf("expected labeled final error, got %q", err.Error())
	}
	if len(err.Error()) > 1400 {
		t.Fatalf("expected sanitized/truncated error, got %d bytes", len(err.Error()))
	}
}

func TestReplicatedWorkspaceReadinessCommandQuotesPaths(t *testing.T) {
	files := map[string]string{
		"AGENTS.md":                "agent instructions",
		"dir/BOOTSTRAP notes.md":   "notes",
		"quote'file.md":            "quoted path",
		"empty-file-is-allowed.md": "",
	}

	cmd := replicatedWorkspaceReadinessCommand("$HOME/.openclaw/workspace", files)

	for name := range files {
		if !strings.Contains(cmd, shellDoubleQuote("$HOME/.openclaw/workspace/"+name)) {
			t.Fatalf("expected quoted path for %q in command:\n%s", name, cmd)
		}
	}
	if strings.Contains(cmd, " -s ") {
		t.Fatalf("readiness must use test -e so empty files are allowed:\n%s", cmd)
	}
	if !strings.Contains(cmd, "test -e") {
		t.Fatalf("expected test -e checks in command:\n%s", cmd)
	}
}

func TestShellDoubleQuoteExpandsHomeButQuotesPathMetacharacters(t *testing.T) {
	got := shellDoubleQuote(`$HOME/.openclaw/workspace/dir/it's "\weird"`)
	if !strings.HasPrefix(got, `"$HOME/`) {
		t.Fatalf("expected $HOME to remain expandable, got %q", got)
	}
	if !strings.Contains(got, `\"\\weird\"`) {
		t.Fatalf("expected embedded quotes and backslash to be escaped, got %q", got)
	}
}

func TestRemoteWriteFileCommandUsesBase64AndCreatesParentDir(t *testing.T) {
	content := "line one\nELASTICCLAW_B64\n$(touch /tmp/should-not-run)\n"
	cmd := remoteWriteFileCommand("$HOME/.openclaw/workspace", "dir/file name.md", content)

	if !strings.Contains(cmd, `mkdir -p -- "$HOME/.openclaw/workspace/dir"`) {
		t.Fatalf("expected parent directory creation, got:\n%s", cmd)
	}
	if !strings.Contains(cmd, `> "$HOME/.openclaw/workspace/dir/file name.md"`) {
		t.Fatalf("expected quoted target path, got:\n%s", cmd)
	}
	if strings.Contains(cmd, content) || strings.Contains(cmd, "$(touch") {
		t.Fatalf("expected content to be base64 encoded, got:\n%s", cmd)
	}
	if !strings.Contains(cmd, base64.StdEncoding.EncodeToString([]byte(content))) {
		t.Fatalf("expected base64 encoded content, got:\n%s", cmd)
	}
}
