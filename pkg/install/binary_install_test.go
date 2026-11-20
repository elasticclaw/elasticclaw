package install

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallBinaryVerification(t *testing.T) {
	for _, mode := range []string{"valid", "mismatch", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			binary := []byte("new binary")
			digest := fmt.Sprintf("%x", sha256.Sum256(binary))
			switch mode {
			case "mismatch":
				digest = strings.Repeat("0", 64)
			case "invalid":
				digest = "not-a-digest"
			}
			for name, data := range map[string][]byte{
				"binary": binary, "installed": []byte("old binary"),
				"curl": []byte("#!/bin/sh\ncp \"$FIXTURE_DIR/binary\" \"$4\"\n"),
			} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(dir, "installed")
			script := strings.ReplaceAll(ScriptInstallBinary("test", digest, false), "/usr/local/bin/elasticclaw", shellQuote(target))
			command := exec.Command("sh", "-c", script)
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE_DIR="+dir)
			out, err := command.CombinedOutput()
			if (err == nil) != (mode == "valid") {
				t.Fatalf("unexpected result: %v\n%s", err, out)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			want := "old binary"
			if mode == "valid" {
				want = string(binary)
			}
			if string(got) != want {
				t.Fatalf("installed %q, want %q", got, want)
			}
		})
	}
}
