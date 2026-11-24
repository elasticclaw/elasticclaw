package hub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaytonaNodeSetupFailsClosed runs the real prefix against a fake nvm and
// node: the payload must only run once node satisfies the range.
func TestDaytonaNodeSetupFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	cases := []struct {
		name        string
		nvm         bool
		node        string // "ok", "vulnerable" or "" (missing)
		installFail bool
		wantPayload bool
	}{
		{name: "compatible node", nvm: true, node: "ok", wantPayload: true},
		{name: "vulnerable node upgraded by nvm", nvm: true, node: "vulnerable", wantPayload: true},
		{name: "missing node installed by nvm", nvm: true, wantPayload: true},
		{name: "nvm install fails", nvm: true, node: "vulnerable", installFail: true},
		{name: "no nvm and vulnerable node", node: "vulnerable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			nvmDir := filepath.Join(dir, "nvm")
			bin := filepath.Join(dir, "bin")
			state := filepath.Join(dir, "node-state")
			for _, d := range []string{nvmDir, bin} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The fake node lives on PATH only once state exists, like a real install.
			writeFakeFile(t, filepath.Join(dir, "node"), "#!/bin/sh\nif [ \"$1\" = -e ]; then [ \"$(cat \"$FAKE_NODE_STATE\")\" = ok ]; exit $?; fi\necho v-$(cat \"$FAKE_NODE_STATE\")\n", 0o755)
			if tc.node != "" {
				writeFakeFile(t, state, tc.node, 0o644)
				if err := os.Symlink(filepath.Join(dir, "node"), filepath.Join(bin, "node")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.nvm {
				install := `printf ok > "$FAKE_NODE_STATE"; ln -sf "$FAKE_NODE" "$FAKE_BIN/node"`
				if tc.installFail {
					install = "return 3"
				}
				writeFakeFile(t, filepath.Join(nvmDir, "nvm.sh"), "nvm() { case \"$1\" in use) return 0;; install) "+install+";; esac; }\n", 0o644)
			}

			prefix := strings.NewReplacer("/usr/local/share/nvm", nvmDir, "/home/daytona", dir).Replace(daytonaNodeSetup)
			cmd := exec.Command("bash", "-c", prefix+"echo PAYLOAD")
			cmd.Env = append(os.Environ(),
				"PATH="+bin+":/usr/bin:/bin",
				"FAKE_NODE_STATE="+state,
				"FAKE_NODE="+filepath.Join(dir, "node"),
				"FAKE_BIN="+bin,
			)
			out, err := cmd.Output()
			ran := strings.Contains(string(out), "PAYLOAD")
			if tc.wantPayload {
				if err != nil || !ran {
					t.Fatalf("payload did not run: err=%v out=%q", err, out)
				}
				return
			}
			if err == nil || ran {
				t.Fatalf("payload ran on an incompatible node: err=%v out=%q", err, out)
			}
			if !strings.Contains(string(out), "CVE-2026-111497") {
				t.Fatalf("stdout %q does not explain the failure", out)
			}
		})
	}
}

func writeFakeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
