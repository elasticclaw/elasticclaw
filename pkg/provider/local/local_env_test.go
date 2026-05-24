package local

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCreateWritesPrivateEnvFileAndExecParsesLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell env behavior is unix-specific")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	p := New()
	_, err := p.Create(context.Background(), types.CreateRequest{
		Name: "env-test",
		Env: map[string]string{
			"A": "1",
			"B": "two:with:colon",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	envFile := filepath.Join(home, ".elasticclaw", "state", "local-instances", "env-test", ".env")
	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("env file mode = %v, want no group/other bits", info.Mode().Perm())
	}

	result, err := p.Exec(context.Background(), "env-test", []string{"sh", "-c", `printf "%s/%s" "$A" "$B"`})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "1/two:with:colon" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}
