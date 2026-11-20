package cliversion

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNixInstallerScript(t *testing.T) {
	for _, want := range []string{NixInstallerVersion, NixInstallerAMD64SHA256, NixInstallerARM64SHA256, "sha256sum -c -", "curl -fsSL --proto '=https'", `install linux --no-confirm --init "$NIX_INIT_MODE"`, "mktemp", "trap"} {
		if !strings.Contains(NixInstallerScript, want) {
			t.Fatalf("missing %q", want)
		}
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(NixInstallerScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestVerifiedAPTKeyScript(t *testing.T) {
	for _, tc := range []struct {
		name, info    string
		failGPG, want bool
	}{
		{name: "primary matches", info: "pub:::::::::\nfpr:::::::::" + DockerKeyFingerprint + ":\n", want: true},
		{name: "wrong primary", info: "pub:::::::::\nfpr:::::::::WRONG:\n"},
		{name: "only subkey matches", info: "pub:::::::::\nfpr:::::::::WRONG:\nsub:::::::::\nfpr:::::::::" + DockerKeyFingerprint + ":\n"},
		{name: "extra primary key", info: "pub:::::::::\nfpr:::::::::" + DockerKeyFingerprint + ":\npub:::::::::\nfpr:::::::::ATTACKER:\n"},
		{name: "gpg failure", failGPG: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"curl": "#!/bin/sh\nexit 0\n",
				"gpg":  "#!/bin/sh\nif [ \"$2\" = --show-keys ]; then printf '%s' \"$KEY_INFO\"; exit \"$GPG_EXIT\"; fi\ntouch \"$DEARMOR_MARKER\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(dir, "dearmored")
			status := "0"
			if tc.failGPG {
				status = "1"
			}
			cmd := exec.Command("bash", "-c", VerifiedAPTKeyScript("https://example.test/key", "/unused", DockerKeyFingerprint, ""))
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "KEY_INFO="+tc.info, "GPG_EXIT="+status, "DEARMOR_MARKER="+marker)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("success = %v, want %v: %s", err == nil, tc.want, out)
			}
			_, statErr := os.Stat(marker)
			if (statErr == nil) != tc.want {
				t.Fatal("unexpected dearmor execution")
			}
		})
	}
}

func TestAgentDockerfileUsesVerifiedNodeSourceKey(t *testing.T) {
	data, err := os.ReadFile("../../docker/agent.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{NodeSourceKeyFingerprint, "--show-keys --with-colons", "signed-by=/etc/apt/keyrings/nodesource.gpg", "node_24.x nodistro main"} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(script, "setup_24.x") {
		t.Fatal("remote setup script remains")
	}
	// Check each RUN command as shell syntax without building or downloading.
	lines := strings.Split(script, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "RUN ") {
			continue
		}
		command := strings.TrimPrefix(lines[i], "RUN ")
		for strings.HasSuffix(command, "\\") && i+1 < len(lines) {
			i++
			command = strings.TrimSuffix(command, "\\") + lines[i]
		}
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(command)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("RUN syntax: %v: %s", err, out)
		}
	}
}
