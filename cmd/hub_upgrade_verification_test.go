package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubUpgradeScriptVerification(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "elasticclaw")
			binary := []byte("#!/bin/sh\necho new-version\n")
			for name, content := range map[string][]byte{
				"binary":      binary,
				"elasticclaw": []byte("#!/bin/sh\necho old-version\n"),
				"curl":        []byte("#!/bin/sh\ncp \"$FIXTURE_DIR/binary\" \"$4\"\n"),
				"sha256sum":   []byte("#!/bin/sh\nshasum -a 256 \"$@\"\n"),
				"systemctl":   []byte("#!/bin/sh\nexit 1\n"),
			} {
				if err := os.WriteFile(filepath.Join(dir, name), content, 0700); err != nil {
					t.Fatal(err)
				}
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(binary))
			if !valid {
				digest = strings.Repeat("0", 64)
			}
			marker := filepath.Join(dir, "injected")
			malicious := "test'$(touch " + marker + ")"
			command := exec.Command("bash", "-c", hubUpgradeScript(malicious, "https://example.com/"+malicious, digest, false))
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE_DIR="+dir)
			out, err := command.CombinedOutput()
			if (err == nil) != valid {
				t.Fatalf("unexpected result: %v\n%s", err, out)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			want := "#!/bin/sh\necho old-version\n"
			if valid {
				want = string(binary)
			}
			if string(got) != want {
				t.Fatalf("installed %q, want %q", got, want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("shell input was executed")
			}
		})
	}
}
