//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	daytonaProvider "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	replicatedProvider "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCleanupRecordedDaytonaSandboxes(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_DAYTONA_SANDBOX_ID_FILE"))
	if path == "" {
		t.Skip("ELASTICCLAW_E2E_DAYTONA_SANDBOX_ID_FILE is not set")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read recorded Daytona sandbox ids: %v", err)
	}
	ids := uniqueNonEmptyLines(string(data))
	if len(ids) == 0 {
		return
	}

	provider, err := daytonaProvider.New(map[string]interface{}{"api_key": os.Getenv("DAYTONA_API_KEY")})
	if err != nil {
		t.Fatalf("create Daytona provider for recorded cleanup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		remaining := make([]string, 0, len(ids))
		for _, id := range ids {
			if err := provider.Destroy(ctx, id, false); err != nil {
				if isBenignDaytonaDeleteError(err) {
					continue
				}
				if !isRetryableDaytonaDeleteError(err) && time.Now().After(deadline) {
					t.Fatalf("delete recorded Daytona sandbox %s: %v", id, err)
				}
			}
			status, err := provider.Status(ctx, id)
			if err != nil {
				if isBenignDaytonaDeleteError(err) {
					continue
				}
				t.Fatalf("check recorded Daytona sandbox %s deletion: %v", id, err)
			}
			if status != types.StatusNotFound {
				remaining = append(remaining, id)
			}
		}
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for recorded Daytona sandboxes to terminate: %s", strings.Join(remaining, ", "))
		}
		time.Sleep(5 * time.Second)
	}
}

func TestCleanupRecordedReplicatedVMs(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_REPLICATED_VM_ID_FILE"))
	if path == "" {
		t.Skip("ELASTICCLAW_E2E_REPLICATED_VM_ID_FILE is not set")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read recorded Replicated VM ids: %v", err)
	}
	ids := uniqueNonEmptyLines(string(data))
	if len(ids) == 0 {
		return
	}

	provider, err := replicatedProvider.New(replicatedProvider.Config{
		Token:       os.Getenv("REPLICATED_API_TOKEN"),
		APIURL:      os.Getenv("ELASTICCLAW_E2E_REPLICATED_API_URL"),
		DefaultType: envOrDefault("ELASTICCLAW_E2E_REPLICATED_INSTANCE_TYPE", "r1.small"),
		DefaultTTL:  envOrDefault("ELASTICCLAW_E2E_REPLICATED_TTL", "1h"),
	})
	if err != nil {
		t.Fatalf("create Replicated provider for recorded cleanup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, id := range ids {
		if err := provider.Destroy(ctx, id, false); err != nil && !isBenignReplicatedDeleteError(err) {
			t.Fatalf("delete recorded Replicated VM %s: %v", id, err)
		}
	}
}

func uniqueNonEmptyLines(data string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}
