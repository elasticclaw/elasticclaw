package hub

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	gatewayLogCaptureLimit   = 256 * 1024
	gatewayLogCaptureTimeout = 20 * time.Second
)

// gatewayLogExecutor is deliberately limited to the operation needed for
// diagnostic capture so this path can be tested without contacting Daytona.
type gatewayLogExecutor interface {
	ExecWithTimeout(context.Context, string, []string, time.Duration) (*types.ExecResult, error)
}

// captureGatewayLog preserves a small tail of a Daytona gateway log before its
// sandbox is destroyed. Capture is diagnostic-only: failures must never affect
// termination.
func (s *Server) captureGatewayLog(clawID, provider, providerID string) {
	if provider != "daytona" {
		return
	}

	s.mu.RLock()
	var cfg types.ProviderConfig
	var ok bool
	if s.hubCfg != nil {
		cfg, ok = s.hubCfg.Providers[provider]
	}
	s.mu.RUnlock()
	if !ok {
		log.Printf("[gateway-log] capture failed for %s: provider is not configured", shortID(clawID))
		return
	}

	executor, err := newDaytonaProvider(cfg)
	if err == nil {
		err = captureGatewayLogWithExecutor(context.Background(), executor, clawID, providerID, hubDataDir())
	}
	if err != nil {
		log.Printf("[gateway-log] capture failed for %s: %v", shortID(clawID), err)
	}
}

func captureGatewayLogWithExecutor(ctx context.Context, executor gatewayLogExecutor, clawID, providerID, dataDir string) error {
	ctx, cancel := context.WithTimeout(ctx, gatewayLogCaptureTimeout)
	defer cancel()

	result, err := executor.ExecWithTimeout(ctx, providerID, []string{"bash", "-lc", "tail -c 262144 ~/.openclaw/gateway.log"}, gatewayLogCaptureTimeout)
	if err != nil {
		return err
	}
	if result == nil {
		return fmt.Errorf("empty exec result")
	}

	contents := []byte(result.Stdout)
	if len(contents) > gatewayLogCaptureLimit {
		contents = contents[len(contents)-gatewayLogCaptureLimit:]
	}

	diagnosticsDir := filepath.Join(dataDir, "diagnostics")
	if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
		return fmt.Errorf("create diagnostics directory: %w", err)
	}
	filename := fmt.Sprintf("%s-%s.log", filepath.Base(clawID), time.Now().UTC().Format(time.RFC3339Nano))
	path := filepath.Join(diagnosticsDir, filename)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create capture file: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write capture file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close capture file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure capture file: %w", err)
	}
	log.Printf("[gateway-log] captured %d bytes for %s at %s", len(contents), shortID(clawID), path)
	return nil
}
