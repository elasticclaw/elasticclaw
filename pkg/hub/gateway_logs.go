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
	gatewayLogCaptureCommand = "tail -c 262144 /home/daytona/.openclaw/gateway.log 2>/dev/null"
	bridgeLogCaptureCommand  = "tail -c 262144 /home/daytona/claw-bridge.log 2>/dev/null"
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
	var firstErr error
	keepErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, capture := range []struct {
		name    string
		command string
	}{
		{name: "gateway", command: gatewayLogCaptureCommand},
		{name: "bridge", command: bridgeLogCaptureCommand},
	} {
		if ctx.Err() != nil {
			keepErr(ctx.Err())
			break
		}
		// Each capture gets its own time box. Sharing one would let a hung tail
		// against a half-dead sandbox starve the second file — and the bridge log
		// is the one the NEXT-655 post-mortem proved we need most.
		execCtx, cancel := context.WithTimeout(ctx, gatewayLogCaptureTimeout)
		// ExecWithTimeout wraps this single script argument in `bash -c`; do not
		// add a second shell here.
		result, err := executor.ExecWithTimeout(execCtx, providerID, []string{capture.command}, gatewayLogCaptureTimeout)
		cancel()
		if err != nil {
			// A transport failure means the hub could not reach the sandbox at all
			// — bad credentials, a network error, a rate limit. That is worth
			// surfacing, unlike a missing log file.
			keepErr(fmt.Errorf("%s log: %w", capture.name, err))
			continue
		}
		// A missing log or an already-dead sandbox is the normal case on teardown:
		// stay quiet, and write nothing rather than an empty file that would read
		// as a successful but blank post-mortem.
		if result == nil || result.ExitCode != 0 || len(result.Stdout) == 0 {
			continue
		}
		if err := writeGatewayLogCapture(dataDir, clawID, capture.name, []byte(result.Stdout)); err != nil {
			keepErr(err)
		}
	}
	return firstErr
}

func writeGatewayLogCapture(dataDir, clawID, source string, contents []byte) error {
	if len(contents) > gatewayLogCaptureLimit {
		contents = contents[len(contents)-gatewayLogCaptureLimit:]
	}

	diagnosticsDir := filepath.Join(dataDir, "diagnostics")
	if err := os.MkdirAll(diagnosticsDir, 0o700); err != nil {
		return fmt.Errorf("create diagnostics directory: %w", err)
	}
	filename := fmt.Sprintf("%s-%s-%s.log", filepath.Base(clawID), time.Now().UTC().Format(time.RFC3339Nano), source)
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
	log.Printf("[gateway-log] captured %s log: %d bytes for %s at %s", source, len(contents), shortID(clawID), path)
	return nil
}
