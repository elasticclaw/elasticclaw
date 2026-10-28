package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNodeGitInstallScriptRechecksNode runs the real install script against
// fake sudo/apt/node: it must fail when the install leaves an incompatible
// node on PATH instead of reporting success.
func TestNodeGitInstallScriptRechecksNode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	cases := []struct {
		name          string
		node          string // initial fake node state: "ok" or "vulnerable"
		aptUpgrades   bool
		wantSuccess   bool
		wantNoInstall bool
	}{
		{name: "compatible node skips apt", node: "ok", wantSuccess: true, wantNoInstall: true},
		{name: "apt upgrades vulnerable node", node: "vulnerable", aptUpgrades: true, wantSuccess: true},
		{name: "apt leaves vulnerable node", node: "vulnerable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "node-state")
			aptLog := filepath.Join(dir, "apt.log")
			fake := map[string]string{
				"node": "#!/bin/sh\nif [ \"$1\" = -e ]; then [ \"$(cat \"$FAKE_NODE_STATE\")\" = ok ]; exit $?; fi\necho v-$(cat \"$FAKE_NODE_STATE\")\n",
				"npm":  "#!/bin/sh\n",
				"git":  "#!/bin/sh\necho git-fake\n",
				"curl": "#!/bin/sh\n",
				// Drain stdin like the real `sudo tee`/`sudo gpg`, or pipefail sees SIGPIPE.
				"sudo": "#!/bin/sh\ncat > /dev/null\necho \"$*\" >> \"$FAKE_APT_LOG\"\ncase \"$*\" in *'install -y --fix-broken nodejs'*) [ -n \"$FAKE_APT_UPGRADES\" ] && printf ok > \"$FAKE_NODE_STATE\";; esac\nexit 0\n",
			}
			for name, body := range fake {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(state, []byte(tc.node), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", nodeGitInstallScript)
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "FAKE_NODE_STATE="+state, "FAKE_APT_LOG="+aptLog)
			if tc.aptUpgrades {
				cmd.Env = append(cmd.Env, "FAKE_APT_UPGRADES=1")
			}
			out, err := cmd.CombinedOutput()
			apt, _ := os.ReadFile(aptLog)
			if tc.wantNoInstall && len(apt) > 0 {
				t.Fatalf("compatible node still ran apt: %q", apt)
			}
			if tc.wantSuccess {
				if err != nil {
					t.Fatalf("install failed: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("install succeeded with an incompatible node:\n%s", out)
			}
			if !strings.Contains(string(out), "CVE-2026-111497") {
				t.Fatalf("output does not explain the failure:\n%s", out)
			}
		})
	}
}
