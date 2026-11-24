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

func TestDaytonaUninstallGlobalOpenClaw(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	if strings.Contains(daytonaUninstallGlobalOpenClawCommand, "\" uninstall") {
		t.Fatal("uninstall command must not walk the npm tree")
	}
	run := func(t *testing.T, prefix string) (string, string) {
		t.Helper()
		root := t.TempDir()
		fakeBin := filepath.Join(root, "fakebin")
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		// sudo runs the command as-is; npm prints the configured prefix.
		writeExecutable(t, fakeBin, "sudo", "#!/bin/sh\nexec \"$@\"\n")
		npm := filepath.Join(root, "npm")
		writeExecutable(t, root, "npm", "#!/bin/sh\nprintf '%s' \""+prefix+"\"\n")
		cmd := strings.Replace(daytonaUninstallGlobalOpenClawCommand, "/usr/local/share/nvm/current/bin/npm", npm, 1)
		c := exec.Command("bash", "-c", cmd)
		c.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("uninstall: %v: %s", err, out)
		}
		return root, string(out)
	}

	t.Run("removes package and all its bin links", func(t *testing.T) {
		prefix := t.TempDir()
		pkg := filepath.Join(prefix, "lib", "node_modules", "openclaw")
		other := filepath.Join(prefix, "lib", "node_modules", "other")
		for _, d := range []string{pkg, other, filepath.Join(prefix, "bin")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		links := map[string]string{
			"openclaw":     "../lib/node_modules/openclaw/openclaw.mjs",
			"openclaw-aux": "../lib/node_modules/openclaw/aux.mjs",
			"other":        "../lib/node_modules/other/cli.js",
		}
		for name, target := range links {
			if err := os.Symlink(target, filepath.Join(prefix, "bin", name)); err != nil {
				t.Fatal(err)
			}
		}
		run(t, prefix)
		if _, err := os.Stat(pkg); !os.IsNotExist(err) {
			t.Errorf("package dir still present: %v", err)
		}
		for _, name := range []string{"openclaw", "openclaw-aux"} {
			if _, err := os.Lstat(filepath.Join(prefix, "bin", name)); !os.IsNotExist(err) {
				t.Errorf("bin link %s still present", name)
			}
		}
		if _, err := os.Lstat(filepath.Join(prefix, "bin", "other")); err != nil {
			t.Errorf("unrelated bin link removed: %v", err)
		}
	})

	t.Run("skips when the prefix is empty", func(t *testing.T) {
		_, out := run(t, "")
		if !strings.Contains(out, "skip: npm prefix unavailable") {
			t.Fatalf("expected skip, got: %s", out)
		}
	})
}
