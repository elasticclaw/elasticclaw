// Provider-agnostic claw provisioning: dispatch, bootstrap status, and shared helpers.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// Provision creates or updates the default tenant (for alpha single-user setup).
// If a tenant named "default" already exists, its token and claw_token are updated
// so that hub.yaml token changes take effect on restart without manual DB surgery.
func (s *Server) Provision(token, clawToken string) (string, error) {
	var existingID string
	_ = s.db.QueryRow(`SELECT id FROM tenants WHERE name = 'default'`).Scan(&existingID)
	if existingID != "" {
		_, err := s.db.Exec(
			`UPDATE tenants SET token = ?, claw_token = ? WHERE id = ?`,
			token, clawToken, existingID,
		)
		if err != nil {
			return "", fmt.Errorf("provision update: %w", err)
		}
		return existingID, nil
	}
	id := uuid.New().String()
	_, err := s.db.Exec(
		`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		id, "default", token, clawToken, now(),
	)
	if err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	return id, nil
}

func recordE2EDaytonaSandboxID(sandboxID string) {
	recordE2EProviderID("Daytona sandbox", "ELASTICCLAW_E2E_DAYTONA_SANDBOX_ID_FILE", sandboxID)
}

func recordE2EReplicatedVMID(vmID string) {
	recordE2EProviderID("Replicated VM", "ELASTICCLAW_E2E_REPLICATED_VM_ID_FILE", vmID)
}

func recordE2EProviderID(label, envName, id string) {
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" || strings.TrimSpace(id) == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			logf("[e2e] record %s id: mkdir %s: %v", label, dir, err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		logf("[e2e] record %s id: open %s: %v", label, path, err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, id); err != nil {
		logf("[e2e] record %s id: write %s: %v", label, path, err)
	}
}

func shellDoubleQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if i == 0 && strings.HasPrefix(s, "$HOME") && (len(s) == len("$HOME") || s[len("$HOME")] == '/') {
			b.WriteString("$HOME")
			i += len("$HOME") - 1
			continue
		}
		switch s[i] {
		case '\\', '"', '`', '$':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func (s *Server) setBootstrapStatus(clawID, status string) {
	s.setBootstrapStatusWithDiagnostic(clawID, status, "")
}

func repoDirectoryName(repoFullName string) string {
	repoParts := strings.SplitN(repoFullName, "/", 2)
	if len(repoParts) == 2 {
		return repoParts[1]
	}
	return repoFullName
}

func (s *Server) markBootstrapReady(clawID string) {
	if clawID == "" {
		return
	}
	_, _ = s.db.Exec(`UPDATE claws SET bootstrap_ok=1, bootstrap_diagnostic='' WHERE id=?`, clawID)
	s.promoteBootstrapReadyClaw(clawID)
}

func (s *Server) promoteBootstrapReadyClaw(clawID string) bool {
	cc := s.clawReg.Lookup(clawID)
	if cc == nil {
		return false
	}

	cc.Mu.RLock()
	gatewayReady := cc.GatewayReady
	tenantID := cc.TenantID
	cc.Mu.RUnlock()
	if !gatewayReady {
		return false
	}

	res, err := s.db.Exec(`UPDATE claws SET status='connected', bootstrap_status='' WHERE id=? AND status='starting' AND bootstrap_ok=1`, clawID)
	if err != nil {
		return false
	}
	rowsUpdated, _ := res.RowsAffected()
	if rowsUpdated == 0 {
		return false
	}

	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "connected"},
	})
	logf("[bridge] ✓ ready after bootstrap: %s", clawID[:8])
	go s.requestBootstrapCheckpoint(clawID)
	s.startWorkflowAfterVolumes(s.base(), cc, clawID)
	return true
}

func (s *Server) startWorkflowAfterVolumes(ctx context.Context, cc *clawConn, clawID string) {
	if cc == nil {
		return
	}
	cc.Mu.Lock()
	if cc.WorkflowStartPending || cc.WorkflowStartDone {
		cc.Mu.Unlock()
		return
	}
	cc.WorkflowStartPending = true
	cc.Mu.Unlock()

	go func() {
		if err := s.attachWorkflowVolumes(ctx, cc, clawID); err != nil {
			cc.Mu.Lock()
			cc.WorkflowStartPending = false
			cc.Mu.Unlock()
			logfCtx(ctx, "[volume] attach workflow volumes for %s failed: %v", clawID[:8], err)
			s.releaseWorkflowVolumeLeases(clawID)
			go s.stopAgentWithReason(clawID, fmt.Sprintf("Workflow volume attach failed: %v", err), false)
			return
		}

		cc.Mu.Lock()
		cc.WorkflowStartPending = false
		cc.WorkflowStartDone = true
		cc.Mu.Unlock()

		if s.initializePipelineEntryIfNeeded(clawID) {
			s.sendInitialPlanInstruction(cc, clawID)
		} else if s.getPipelineStage(clawID) == "" && !s.clawHasMessages(clawID) {
			s.sendWakeMessage(cc, clawID)
		}
	}()
}

func (s *Server) setBootstrapStatusWithDiagnostic(clawID, status, diagnostic string) {
	if clawID == "" {
		return
	}
	res, err := s.db.Exec(`UPDATE claws SET bootstrap_status=?, bootstrap_diagnostic=? WHERE id=? AND status != 'deleted'`, status, diagnostic, clawID)
	if err != nil {
		return
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return
	}

	var tenantID string
	_ = s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=? AND status != 'deleted'`, clawID).Scan(&tenantID)
	if tenantID == "" {
		return
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "claw_status",
		Payload: map[string]string{
			"claw_id":              clawID,
			"status":               "starting",
			"bootstrap_status":     status,
			"bootstrap_diagnostic": diagnostic,
		},
	})
}

func formatRetryDelay(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func sanitizeBootstrapOutput(out string) string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	lines := strings.Split(out, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "declare -x ") {
			continue
		}
		cleaned = append(cleaned, trimmed)
	}
	result := strings.TrimSpace(strings.Join(cleaned, "\n"))
	if result == "" {
		return "no command output"
	}
	const maxLen = 1200
	if len(result) <= maxLen {
		return result
	}
	return result[len(result)-maxLen:]
}

func sanitizeBootstrapError(err error) string {
	if err == nil {
		return "unknown error"
	}
	return sanitizeBootstrapOutput(err.Error())
}

// ─── Bootstrap ────────────────────────────────────────────────────────────────

const githubReleasesBase = "https://github.com/elasticclaw/elasticclaw/releases/download"

// Version is set by cmd at startup so the hub can construct versioned download URLs.
var Version = "dev"

// bridgeDownloadURL returns the URL to download the claw-bridge binary.
// Uses hub.yaml bridge_image if set, otherwise constructs the GitHub releases URL
// from the hub's own version. Returns empty string if version is 'dev' and no
// bridge_image is configured — caller must check and fail appropriately.
func (s *Server) bridgeDownloadURL() string {
	if s.hubCfg.BridgeImage != "" {
		return s.hubCfg.BridgeImage
	}
	if Version == "dev" || Version == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/claw-bridge-linux-amd64", githubReleasesBase, Version)
}

// randomHex returns a random hex string of n bytes (2*n hex chars).
// mergeTags combines tags from all sources in priority order:
// 1. auto tag (template:<name>)
// 2. template config tags (elasticclaw-config.yaml)
// 3. CLI --tag flags
// Deduplicates while preserving order.
var clawColors = []string{
	"slate", "red", "orange", "amber", "lime", "green", "emerald", "teal",
	"cyan", "sky", "blue", "indigo", "violet", "purple", "pink", "rose",
}

var clawColorSet = func() map[string]bool {
	m := make(map[string]bool, len(clawColors))
	for _, c := range clawColors {
		m[c] = true
	}
	return m
}()

// resolveColor returns the color for a claw.
// Uses the requested color if valid, otherwise auto-assigns from the claw name.
func resolveColor(requested, clawName string) string {
	if requested != "" && clawColorSet[requested] {
		return requested
	}
	// Hash name → deterministic color
	var h uint32
	for _, c := range clawName {
		h = h*31 + uint32(c)
	}
	return clawColors[h%uint32(len(clawColors))]
}

func mergeTags(templateName string, configTags []string, cliTags []string) []string {
	seen := make(map[string]bool)
	var result []string
	add := func(t string) {
		if t == "" {
			return
		}
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	add("template:" + templateName)
	for _, t := range configTags {
		add(t)
	}
	for _, t := range cliTags {
		add(t)
	}
	return result
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func templateFlakeFiles(files map[string]string) map[string]string {
	flakeFiles := make(map[string]string, 2)
	for _, name := range []string{"flake.nix", "flake.lock"} {
		if content, ok := files[name]; ok {
			flakeFiles[name] = content
		}
	}
	return flakeFiles
}

// clawHubURL returns the URL claws should use to connect back.
// Uses public_url if set, otherwise falls back to url.
func (s *Server) clawHubURL() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.hubCfg.PublicURL != "" {
		return s.hubCfg.PublicURL
	}
	return s.hubCfg.URL
}

// ─── Terminal WebSocket ───────────────────────────────────────────────────────

// handleTerminal proxies a WebSocket connection to an SSH PTY on the claw's VM.
// Route: GET /api/terminal/{clawID}?token=...
// terminateVM terminates a provider VM by type and ID.
func (s *Server) terminateVM(provider, vmID string) {
	if vmID == "" {
		return
	}
	switch provider {
	case "replicated":
		s.terminateReplicatedVM(vmID)
	case "daytona":
		s.terminateDaytonaVM(vmID)
	case "exedev":
		s.terminateExedevVM(vmID)
	case "docker":
		s.terminateDockerVM(vmID)
	case "lambda-microvms":
		s.terminateLambdaMicroVM(vmID)
	default:
		logf("terminateVM: unsupported provider %q for VM %s", provider, vmID)
	}
}
