package main

import (
	"bytes"
	"log"
	"os/exec"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
)

func TestRuntimeInstallScripts(t *testing.T) {
	for _, want := range []string{cliversion.NodeSourceKeyFingerprint, "gpg --batch --show-keys --with-colons", "nodistro main"} {
		if !strings.Contains(nodeGitInstallScript, want) {
			t.Fatalf("missing %q", want)
		}
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(nodeGitInstallScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestWorkspaceFlakeFlags(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	if got := workspaceFlakeFlags(true); got != "--no-write-lock-file" {
		t.Fatalf("locked flags: %q", got)
	}
	if output.Len() != 0 {
		t.Fatal("locked flake warned")
	}
	if got := workspaceFlakeFlags(false); got != "" {
		t.Fatalf("unlocked flags: %q", got)
	}
	if !strings.Contains(output.String(), "[supply-chain] WARNING workspace flake.lock is missing") {
		t.Fatalf("missing warning: %s", &output)
	}
}
