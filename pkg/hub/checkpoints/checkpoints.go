package checkpoints

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
	daytonaProvider "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	exedevProvider "github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	gossh "golang.org/x/crypto/ssh"
)

const (
	checkpointIdleInterval       = 10 * time.Minute
	checkpointMinInterval        = 5 * time.Minute
	checkpointRequestTimeout     = 2 * time.Minute
	checkpointTerminationTimeout = 90 * time.Second
	maxCheckpointBlobBytes       = 256 << 20
)

// RequestTimeout is the default timeout for a checkpoint request; exported
// for the hub call sites that passed checkpointRequestTimeout before the
// extraction.
const RequestTimeout = checkpointRequestTimeout

type checkpointSummary struct {
	ID              string     `json:"id"`
	ClawID          string     `json:"claw_id"`
	Status          string     `json:"status"`
	Reason          string     `json:"reason"`
	CreatedBy       string     `json:"created_by"`
	ManifestSHA256  string     `json:"manifest_sha256,omitempty"`
	RootTreeSHA256  string     `json:"root_tree_sha256,omitempty"`
	MessageSHA256   string     `json:"message_tree_sha256,omitempty"`
	WorkspaceSHA256 string     `json:"workspace_tree_sha256,omitempty"`
	MessageCount    int        `json:"message_count"`
	PRCount         int        `json:"pr_count"`
	RepoCount       int        `json:"repo_count"`
	Error           string     `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type checkpointManifest struct {
	Schema       int                    `json:"schema"`
	CheckpointID string                 `json:"checkpoint_id"`
	ClawID       string                 `json:"claw_id"`
	CreatedAt    time.Time              `json:"created_at"`
	Reason       string                 `json:"reason"`
	Hub          checkpointHubManifest  `json:"hub"`
	Provider     checkpointProvider     `json:"provider"`
	Messages     checkpointMessages     `json:"messages"`
	Workspace    checkpointWorkspace    `json:"workspace"`
	PRs          []checkpointPR         `json:"prs"`
	Files        []types.CheckpointFile `json:"files"`
}

type checkpointHubManifest struct {
	Version           string   `json:"version"`
	Template          string   `json:"template"`
	TemplateFilesSHA  string   `json:"template_files_sha256"`
	DefaultModel      string   `json:"default_model,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	Color             string   `json:"color,omitempty"`
	FactoryName       string   `json:"factory_name,omitempty"`
	ConcurrencyGroup  string   `json:"concurrency_group,omitempty"`
	LinearIssueID     string   `json:"linear_issue_id,omitempty"`
	GitHubIssueID     string   `json:"github_issue_id,omitempty"`
	ShortcutStoryID   string   `json:"shortcut_story_id,omitempty"`
	ExternalTriggerID string   `json:"external_trigger_id,omitempty"`
	PipelineStage     string   `json:"pipeline_stage,omitempty"`
}

type checkpointProvider struct {
	Name         string `json:"name"`
	ProviderID   string `json:"provider_id"`
	InstanceType string `json:"instance_type,omitempty"`
}

type checkpointMessages struct {
	BlobSHA256 string    `json:"blob_sha256"`
	Count      int       `json:"count"`
	CutoffAt   time.Time `json:"cutoff_at,omitempty"`
}

type checkpointWorkspace struct {
	TreeSHA256 string `json:"tree_sha256"`
}

type checkpointPR struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	URL       string `json:"url"`
	LastCISHA string `json:"last_ci_sha,omitempty"`
}

func checkpointsRoot() string {
	return filepath.Join(hubDataDir(), "checkpoints")
}

func checkpointBlobPath(sha string) string {
	clean := strings.TrimPrefix(sha, "sha256:")
	if len(clean) < 4 {
		return filepath.Join(checkpointsRoot(), "blobs", "sha256", clean)
	}
	return filepath.Join(checkpointsRoot(), "blobs", "sha256", clean[:2], clean[2:4], clean)
}

func checkpointManifestPath(id string) string {
	return filepath.Join(checkpointsRoot(), "manifests", id+".json")
}

func (s *Service) CheckpointScheduler() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.requestIdleCheckpoints()
	}
}

func (s *Service) requestIdleCheckpoints() {
	now := time.Now()
	items := s.deps.ConnectedClaws()

	for _, item := range items {
		item.Conn.RLock()
		lastUser := item.Conn.LastUserMessageAtLocked()
		streaming := item.Conn.StreamingLocked()
		inProgress := item.Conn.CheckpointInProgressLocked()
		item.Conn.RUnlock()
		if streaming || inProgress || now.Sub(lastUser) < checkpointIdleInterval {
			continue
		}
		if s.HasRecentCheckpoint(item.ID, checkpointMinInterval) {
			continue
		}
		go func(clawID string) {
			if _, err := s.RequestCheckpoint(s.baseCtx(), clawID, "idle-timer", "hub", false, checkpointRequestTimeout); err != nil {
				logf("[checkpoint] idle request for %s failed: %v", ShortID(clawID), err)
			}
		}(item.ID)
	}
}

func (s *Service) HasRecentCheckpoint(clawID string, minAge time.Duration) bool {
	var completedAt time.Time
	err := s.deps.DB.QueryRow(`SELECT completed_at FROM claw_checkpoints WHERE claw_id=? AND status='ready' ORDER BY completed_at DESC LIMIT 1`, clawID).Scan(&completedAt)
	return err == nil && time.Since(completedAt) < minAge
}

func (s *Service) hasRecentCheckpointReason(clawID, reason string, minAge time.Duration) bool {
	var createdAt time.Time
	err := s.deps.DB.QueryRow(`SELECT created_at FROM claw_checkpoints WHERE claw_id=? AND reason=? AND status IN ('creating','ready') ORDER BY created_at DESC LIMIT 1`, clawID, reason).Scan(&createdAt)
	return err == nil && time.Since(createdAt) < minAge
}

func (s *Service) RequestBootstrapCheckpoint(clawID string) {
	if s.hasRecentCheckpointReason(clawID, "bootstrap", time.Hour) {
		return
	}
	if _, err := s.RequestCheckpoint(s.baseCtx(), clawID, "bootstrap", "hub", false, checkpointRequestTimeout); err != nil {
		logf("[checkpoint] bootstrap request for %s failed: %v", ShortID(clawID), err)
	}
}

func (s *Service) HandleClawCheckpoints(w http.ResponseWriter, r *http.Request, clawID string) {
	tenantID := s.deps.TenantFromRequest(r)
	if r.Method == http.MethodPost && s.deps.GithubLoginFromContext(r.Context()) != "" {
		var tagsJSON string
		if err := s.deps.DB.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&tagsJSON); err != nil {
			httpserver.WriteErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		var tags []string
		_ = json.Unmarshal([]byte(tagsJSON), &tags)
		s.deps.CfgMu.RLock()
		var accessCfg *types.AccessConfig
		if s.hubCfg().Auth != nil {
			accessCfg = s.hubCfg().Auth.Access
		}
		s.deps.CfgMu.RUnlock()
		if !s.deps.CanModifyClaw(accessCfg, s.deps.GithubLoginFromContext(r.Context()), tags) {
			httpserver.WriteErr(w, http.StatusForbidden, "forbidden", "forbidden")
			return
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/claws/"), "/")
	if len(parts) == 4 && parts[1] == "checkpoints" && parts[3] == "restore" {
		if r.Method != http.MethodPost {
			httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if err := s.restoreClawFromCheckpoint(r.Context(), tenantID, clawID, parts[2]); err != nil {
			httpserver.WriteDomainErr(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		httpserver.JSONOK(w, map[string]string{"status": "restoring", "checkpoint_id": parts[2]})
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.deps.DB.Query(`SELECT id, claw_id, status, reason, created_by, manifest_sha256, root_tree_sha256, message_tree_sha256, workspace_tree_sha256, message_count, pr_count, repo_count, error, created_at, completed_at FROM claw_checkpoints WHERE tenant_id=? AND claw_id=? ORDER BY created_at DESC`, tenantID, clawID)
		if err != nil {
			httpserver.WriteErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		defer rows.Close()
		var out []checkpointSummary
		for rows.Next() {
			var c checkpointSummary
			var completed sql.NullTime
			if err := rows.Scan(&c.ID, &c.ClawID, &c.Status, &c.Reason, &c.CreatedBy, &c.ManifestSHA256, &c.RootTreeSHA256, &c.MessageSHA256, &c.WorkspaceSHA256, &c.MessageCount, &c.PRCount, &c.RepoCount, &c.Error, &c.CreatedAt, &completed); err != nil {
				continue
			}
			if completed.Valid {
				c.CompletedAt = &completed.Time
			}
			out = append(out, c)
		}
		if out == nil {
			out = []checkpointSummary{}
		}
		httpserver.JSONOK(w, out)
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reason := body.Reason
		if reason == "" {
			reason = "manual"
		}
		id, err := s.RequestCheckpoint(r.Context(), clawID, reason, "user", false, checkpointRequestTimeout)
		if err != nil {
			httpserver.WriteErr(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
		httpserver.JSONOK(w, map[string]string{"id": id, "status": "creating"})
		return
	}
	httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func (s *Service) restoreClawFromCheckpoint(ctx context.Context, tenantID, clawID, checkpointID string) error {
	var status, manifestPath string
	if err := s.deps.DB.QueryRow(`SELECT status, manifest_path FROM claw_checkpoints WHERE id=? AND tenant_id=? AND claw_id=?`, checkpointID, tenantID, clawID).Scan(&status, &manifestPath); err != nil {
		if err == sql.ErrNoRows {
			return types.ErrCheckpointNotFound
		}
		return err
	}
	if status != "ready" {
		return types.ErrCheckpointNotReady
	}
	if manifestPath == "" {
		return fmt.Errorf("checkpoint has no manifest: %w", types.ErrCheckpointNotReady)
	}

	// Preserve the current state if the bridge is still reachable.
	s.CheckpointBeforeTermination(clawID, "pre-reset")

	var provider, providerID string
	if err := s.deps.DB.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&provider, &providerID); err != nil {
		return err
	}
	s.deps.CloseClawConn(clawID, 1000, "restoring checkpoint")
	if providerID != "" {
		go s.terminateVM(provider, providerID)
	}

	_, err := s.deps.DB.Exec(`UPDATE claws SET status='provisioning', bootstrap_ok=0, bootstrap_status='Restoring checkpoint', provider_id='', restore_checkpoint_id=?, restored_from_checkpoint_id=? WHERE id=? AND tenant_id=?`,
		checkpointID, checkpointID, clawID, tenantID)
	if err != nil {
		return err
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "provisioning", "bootstrap_status": "Restoring checkpoint"},
	})
	go s.provisionStoredClaw(clawID)
	return nil
}

func (s *Service) provisionStoredClaw(clawID string) {
	var (
		tenantID, name, template, provider, defaultModel, templateFilesJSON, githubReposJSON, linearWorkspace, llmKey string
		nixEnabled, dockerEnabled                                                                                     int
	)
	err := s.deps.DB.QueryRow(
		`SELECT tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, llm_key FROM claws WHERE id=?`,
		clawID,
	).Scan(&tenantID, &name, &template, &provider, &defaultModel, &templateFilesJSON, &githubReposJSON, &linearWorkspace, &nixEnabled, &dockerEnabled, &llmKey)
	if err != nil {
		logf("[restore] failed to load claw %s: %v", ShortID(clawID), err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore failed: %v", err), false)
		return
	}
	var templateFiles map[string]string
	_ = json.Unmarshal([]byte(templateFilesJSON), &templateFiles)
	s.deps.CfgMu.RLock()
	clawToken := s.hubCfg().ClawToken
	provCfg, ok := s.hubCfg().Providers[provider]
	hubSecrets := s.hubCfg().Secrets
	s.deps.CfgMu.RUnlock()
	if !ok {
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore failed: provider %q is not configured", provider), false)
		return
	}
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":    clawID,
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		if parsed, parseErr := config.ParseTemplateConfig([]byte(cfgContent)); parseErr == nil {
			tmplCfg = parsed
			for envName, secretRef := range parsed.SecretRefs {
				if val, ok := hubSecrets[secretRef]; ok {
					env[envName] = val
				}
			}
		}
	}
	templateFiles = s.deps.InjectFigmaAPIDocs(templateFiles, env)
	fileBytes := make(map[string][]byte, len(templateFiles))
	for k, v := range templateFiles {
		fileBytes[k] = []byte(v)
	}
	req := types.CreateClawRequest{
		Name:         name,
		TemplateName: template,
		Provider:     provider,
		DefaultModel: defaultModel,
		Files:        templateFiles,
		Env:          env,
		Nix:          nixEnabled != 0,
		Docker:       dockerEnabled != 0,
		LLMKey:       llmKey,
		ProviderName: "ec-" + clawID[:8] + "-r" + uuid.New().String()[:4],
	}
	if tmplCfg != nil {
		req.InstanceType = tmplCfg.InstanceType
		req.Snapshot = tmplCfg.Snapshot
		req.TTL = tmplCfg.TTL
	}
	ctx := s.baseCtx()
	var provErr error
	switch provider {
	case "daytona":
		provErr = s.provisionDaytona(ctx, clawID, req, provCfg, fileBytes, env)
	case "replicated":
		provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
	case "exedev":
		provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, env)
	case "lambda-microvms":
		provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, fileBytes)
	default:
		provErr = fmt.Errorf("unsupported provider: %s", provider)
	}
	if provErr != nil {
		logf("[restore] provision failed for claw %s: %v", clawID, provErr)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore provision failed: %v", provErr), false)
		return
	}
	var restoreCheckpointID string
	_ = s.deps.DB.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id=?`, clawID).Scan(&restoreCheckpointID)
	_, _ = s.deps.DB.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,format) VALUES(?,?,?,?,?,?,?)`,
		uuid.New().String(), clawID, tenantID, "system", fmt.Sprintf("[hub] Restoring from checkpoint %s.", restoreCheckpointID), now(), "pre")
}

func (s *Service) RequestCheckpoint(ctx context.Context, clawID, reason, createdBy string, wait bool, timeout time.Duration) (string, error) {
	tenantID, provider, providerID, err := s.clawCheckpointIdentity(clawID)
	if err != nil {
		return "", err
	}

	cc := s.deps.ClawConn(clawID)
	if cc == nil {
		checkpointID := uuid.New().String()
		if err := s.InsertCheckpoint(checkpointID, tenantID, clawID, reason, createdBy, provider, providerID); err != nil {
			return "", err
		}
		if err := s.completeMetadataOnlyCheckpoint(checkpointID, clawID, reason, "bridge unreachable"); err != nil {
			return checkpointID, err
		}
		return checkpointID, nil
	}

	cc.Lock()
	busy := cc.StreamingLocked()
	if busy || cc.CheckpointInProgressLocked() {
		if cc.PendingCheckpointIDLocked() != "" {
			stronger := strongerCheckpointReason(cc.PendingCheckpointReasonLocked(), reason)
			if stronger != cc.PendingCheckpointReasonLocked() {
				cc.SetPendingCheckpointReasonLocked(stronger)
				_, _ = s.deps.DB.Exec(`UPDATE claw_checkpoints SET reason=? WHERE id=?`, stronger, cc.PendingCheckpointIDLocked())
			}
			pendingID := cc.PendingCheckpointIDLocked()
			cc.Unlock()
			if wait {
				return pendingID, s.waitCheckpointStatus(ctx, pendingID, timeout)
			}
			return pendingID, nil
		}
		checkpointID := uuid.New().String()
		if err := s.InsertCheckpoint(checkpointID, tenantID, clawID, reason, createdBy, provider, providerID); err != nil {
			cc.Unlock()
			return "", err
		}
		cc.SetPendingCheckpointLocked(checkpointID, reason, createdBy)
		cc.Unlock()
		if wait {
			return checkpointID, s.waitCheckpointStatus(ctx, checkpointID, timeout)
		}
		return checkpointID, nil
	}
	checkpointID := uuid.New().String()
	if err := s.InsertCheckpoint(checkpointID, tenantID, clawID, reason, createdBy, provider, providerID); err != nil {
		cc.Unlock()
		return "", err
	}
	cc.SetCheckpointInProgressLocked(true)
	cc.Unlock()
	return s.DispatchCheckpoint(ctx, cc, clawID, checkpointID, reason, wait, timeout)
}

func (s *Service) waitCheckpointStatus(ctx context.Context, checkpointID string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status, msg string
		_ = s.deps.DB.QueryRow(`SELECT status, error FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&status, &msg)
		switch status {
		case "ready":
			return nil
		case "failed":
			if msg == "" {
				msg = "checkpoint failed"
			}
			return fmt.Errorf("%s", msg)
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) DispatchCheckpoint(ctx context.Context, cc Conn, clawID, checkpointID, reason string, wait bool, timeout time.Duration) (string, error) {
	ch := make(chan error, 1)
	s.deps.CheckpointMu.Lock()
	s.deps.CheckpointWaiters()[checkpointID] = ch
	s.deps.CheckpointMu.Unlock()

	s.deps.CfgMu.RLock()
	clawToken := s.hubCfg().ClawToken
	s.deps.CfgMu.RUnlock()
	payload := types.CheckpointCreatePayload{
		CheckpointID: checkpointID,
		Reason:       reason,
		HubURL:       s.clawHubURL(),
		ClawToken:    clawToken,
	}
	if err := cc.WriteWS(ctx, types.WSMessage{Type: "checkpoint_create", Payload: payload}); err != nil {
		s.finishCheckpointRequest(clawID, checkpointID)
		_ = s.failCheckpoint(checkpointID, err.Error())
		s.notifyCheckpointWaiter(checkpointID, err)
		return checkpointID, err
	}

	if !wait {
		return checkpointID, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case err := <-ch:
		return checkpointID, err
	case <-waitCtx.Done():
		return checkpointID, waitCtx.Err()
	}
}

func (s *Service) DrainPendingCheckpoint(clawID string) {
	cc := s.deps.ClawConn(clawID)
	if cc == nil {
		return
	}
	cc.Lock()
	checkpointID := cc.PendingCheckpointIDLocked()
	reason := cc.PendingCheckpointReasonLocked()
	cc.SetPendingCheckpointLocked("", "", "")
	inProgress := cc.CheckpointInProgressLocked()
	if reason != "" && !inProgress {
		cc.SetCheckpointInProgressLocked(true)
	}
	cc.Unlock()
	if checkpointID == "" || reason == "" || inProgress {
		return
	}
	go func() {
		if _, err := s.DispatchCheckpoint(s.baseCtx(), cc, clawID, checkpointID, reason, false, checkpointRequestTimeout); err != nil {
			logf("[checkpoint] queued request for %s failed: %v", ShortID(clawID), err)
		}
	}()
}

func (s *Service) CheckpointBeforeTermination(clawID, reason string) {
	if _, err := s.RequestCheckpoint(s.baseCtx(), clawID, "termination:"+reason, "hub", true, checkpointTerminationTimeout); err != nil {
		logf("[checkpoint] termination checkpoint for %s failed: %v", ShortID(clawID), err)
	}
}

func strongerCheckpointReason(current, next string) string {
	if current == "" {
		return next
	}
	rank := func(v string) int {
		switch {
		case strings.HasPrefix(v, "termination"):
			return 6
		case v == "pre-reset":
			return 5
		case v == "manual":
			return 4
		case v == "crash-suspected":
			return 3
		case v == "done":
			return 2
		case v == "idle-timer":
			return 1
		default:
			return 0
		}
	}
	if rank(next) > rank(current) {
		return next
	}
	return current
}

func (s *Service) clawCheckpointIdentity(clawID string) (tenantID, provider, providerID string, err error) {
	err = s.deps.DB.QueryRow(`SELECT tenant_id, COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &provider, &providerID)
	return tenantID, provider, providerID, err
}

func (s *Service) InsertCheckpoint(id, tenantID, clawID, reason, createdBy, provider, providerID string) error {
	_, err := s.deps.DB.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, reason, created_by, provider, provider_id_at_create, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, tenantID, clawID, "creating", reason, createdBy, provider, providerID, now())
	return err
}

func (s *Service) HandleCheckpointInternal(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/checkpoints/"), "/")
	if len(parts) != 2 {
		httpserver.WriteErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	checkpointID, action := parts[0], parts[1]
	tenantID, clawID, ok := s.authenticateCheckpointClaw(w, r, checkpointID)
	if !ok {
		return
	}
	switch action {
	case "plan":
		if r.Method != http.MethodPost {
			httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		var plan types.CheckpointPlan
		if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
			httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "invalid plan")
			return
		}
		plan.CheckpointID = checkpointID
		missing := make([]string, 0)
		for _, f := range plan.Files {
			if f.SHA256 == "" {
				continue
			}
			if _, err := os.Stat(checkpointBlobPath(f.SHA256)); os.IsNotExist(err) {
				missing = append(missing, f.SHA256)
			}
		}
		httpserver.JSONOK(w, types.CheckpointPlanAck{Upload: missing})
	case "complete":
		if r.Method != http.MethodPost {
			httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		var complete types.CheckpointComplete
		if err := json.NewDecoder(r.Body).Decode(&complete); err != nil {
			httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "invalid complete")
			return
		}
		if complete.Error != "" {
			_ = s.failCheckpoint(checkpointID, complete.Error)
			s.notifyCheckpointWaiter(checkpointID, fmt.Errorf("%s", complete.Error))
			s.finishCheckpointRequest(clawID, checkpointID)
			s.DrainPendingCheckpoint(clawID)
			httpserver.JSONOK(w, map[string]string{"status": "failed"})
			return
		}
		if err := s.finalizeCheckpoint(checkpointID, tenantID, clawID, complete.RootSHA256); err != nil {
			_ = s.failCheckpoint(checkpointID, err.Error())
			s.notifyCheckpointWaiter(checkpointID, err)
			s.finishCheckpointRequest(clawID, checkpointID)
			s.DrainPendingCheckpoint(clawID)
			httpserver.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		s.notifyCheckpointWaiter(checkpointID, nil)
		s.finishCheckpointRequest(clawID, checkpointID)
		s.DrainPendingCheckpoint(clawID)
		httpserver.JSONOK(w, map[string]string{"status": "ready"})
	default:
		httpserver.WriteErr(w, http.StatusNotFound, "not_found", "not found")
	}
}

func (s *Service) HandleCheckpointBlobUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	sha := strings.TrimPrefix(r.URL.Path, "/api/checkpoints/blob/")
	if !validSHA256(sha) {
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "bad sha")
		return
	}
	token := r.Header.Get("X-Claw-Token")
	if token == "" {
		token = r.URL.Query().Get("claw_token")
	}
	if _, err := s.tenantByClawToken(token); err != nil {
		httpserver.WriteErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCheckpointBlobBytes)
	path := checkpointBlobPath(sha)
	if _, err := os.Stat(path); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		httpserver.WriteErr(w, http.StatusInternalServerError, "internal", "storage error")
		return
	}
	tmp := path + ".tmp-" + uuid.New().String()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		httpserver.WriteErr(w, http.StatusInternalServerError, "internal", "storage error")
		return
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), r.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		var maxBytesErr *http.MaxBytesError
		if errors.As(copyErr, &maxBytesErr) {
			httpserver.WriteErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "blob too large")
			return
		}
		httpserver.WriteErr(w, http.StatusInternalServerError, "internal", "write error")
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		_ = os.Remove(tmp)
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "sha mismatch")
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(path); statErr == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpserver.WriteErr(w, http.StatusInternalServerError, "internal", "storage error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) authenticateCheckpointClaw(w http.ResponseWriter, r *http.Request, checkpointID string) (tenantID, clawID string, ok bool) {
	token := r.Header.Get("X-Claw-Token")
	if token == "" {
		token = r.URL.Query().Get("claw_token")
	}
	resolvedTenant, err := s.tenantByClawToken(token)
	if err != nil {
		httpserver.WriteErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return "", "", false
	}
	err = s.deps.DB.QueryRow(`SELECT tenant_id, claw_id FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&tenantID, &clawID)
	if err == sql.ErrNoRows {
		httpserver.WriteErr(w, http.StatusNotFound, "not_found", "not found")
		return "", "", false
	}
	if err != nil {
		httpserver.WriteErr(w, http.StatusInternalServerError, "internal", "db error")
		return "", "", false
	}
	if tenantID != resolvedTenant {
		httpserver.WriteErr(w, http.StatusForbidden, "forbidden", "forbidden")
		return "", "", false
	}
	return tenantID, clawID, true
}

func (s *Service) finalizeCheckpoint(checkpointID, tenantID, clawID, rootSHA string) error {
	files, err := s.filesForTree(rootSHA)
	if err != nil {
		return err
	}
	msgSHA, msgCount, cutoff, err := s.writeMessageCheckpointBlob(clawID, tenantID)
	if err != nil {
		return err
	}
	manifest, err := s.buildCheckpointManifest(checkpointID, clawID, rootSHA, msgSHA, msgCount, cutoff, files)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestSHA := shaBytes(data)
	path := checkpointManifestPath(checkpointID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return err
	}
	_, err = s.deps.DB.Exec(`UPDATE claw_checkpoints SET status='ready', manifest_sha256=?, manifest_path=?, root_tree_sha256=?, message_tree_sha256=?, workspace_tree_sha256=?, message_count=?, pr_count=?, repo_count=?, completed_at=? WHERE id=?`,
		manifestSHA, path, rootSHA, msgSHA, rootSHA, msgCount, len(manifest.PRs), checkpointRepoCount(manifest.PRs), now(), checkpointID)
	return err
}

func (s *Service) completeMetadataOnlyCheckpoint(checkpointID, clawID, reason, detail string) error {
	tenantID, _, _, err := s.clawCheckpointIdentity(clawID)
	if err != nil {
		return err
	}
	msgSHA, msgCount, cutoff, err := s.writeMessageCheckpointBlob(clawID, tenantID)
	if err != nil {
		return err
	}
	manifest, err := s.buildCheckpointManifest(checkpointID, clawID, "", msgSHA, msgCount, cutoff, nil)
	if err != nil {
		return err
	}
	manifest.Workspace.TreeSHA256 = ""
	data, _ := json.MarshalIndent(manifest, "", "  ")
	manifestSHA := shaBytes(data)
	path := checkpointManifestPath(checkpointID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return err
	}
	_, err = s.deps.DB.Exec(`UPDATE claw_checkpoints SET status='ready', manifest_sha256=?, manifest_path=?, message_tree_sha256=?, message_count=?, error=?, completed_at=? WHERE id=?`,
		manifestSHA, path, msgSHA, msgCount, detail, now(), checkpointID)
	return err
}

func (s *Service) filesForTree(rootSHA string) ([]types.CheckpointFile, error) {
	if rootSHA == "" {
		return nil, nil
	}
	path := checkpointBlobPath(rootSHA)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint tree: %w", err)
	}
	var files []types.CheckpointFile
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("parse checkpoint tree: %w", err)
	}
	return files, nil
}

func (s *Service) writeMessageCheckpointBlob(clawID, tenantID string) (string, int, time.Time, error) {
	rows, err := s.deps.DB.Query(`SELECT id, role, content, format, created_at FROM messages WHERE claw_id=? AND tenant_id=? ORDER BY created_at ASC`, clawID, tenantID)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	defer rows.Close()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	count := 0
	var cutoff time.Time
	for rows.Next() {
		var row struct {
			ID        string    `json:"id"`
			Role      string    `json:"role"`
			Content   string    `json:"content"`
			Format    string    `json:"format,omitempty"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := rows.Scan(&row.ID, &row.Role, &row.Content, &row.Format, &row.CreatedAt); err != nil {
			continue
		}
		cutoff = row.CreatedAt
		count++
		_ = enc.Encode(row)
	}
	sha := shaBytes(buf.Bytes())
	path := checkpointBlobPath(sha)
	if _, err := os.Stat(path); err == nil {
		return sha, count, cutoff, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", 0, time.Time{}, err
	}
	return sha, count, cutoff, os.WriteFile(path, buf.Bytes(), 0o640)
}

func (s *Service) buildCheckpointManifest(checkpointID, clawID, rootSHA, msgSHA string, msgCount int, cutoff time.Time, files []types.CheckpointFile) (*checkpointManifest, error) {
	var row struct {
		TenantID, Template, Provider, ProviderID, DefaultModel, TemplateFiles, Tags, Color                             string
		FactoryName, ConcurrencyGroup, LinearIssueID, GitHubIssueID, ShortcutStoryID, ExternalTriggerID, PipelineStage string
	}
	err := s.deps.DB.QueryRow(`SELECT tenant_id, template, provider, provider_id, default_model, template_files, tags, color, factory_name, concurrency_group, linear_issue_id, github_issue_id, shortcut_story_id, external_trigger_id, pipeline_stage FROM claws WHERE id=?`, clawID).Scan(
		&row.TenantID, &row.Template, &row.Provider, &row.ProviderID, &row.DefaultModel, &row.TemplateFiles, &row.Tags, &row.Color,
		&row.FactoryName, &row.ConcurrencyGroup, &row.LinearIssueID, &row.GitHubIssueID, &row.ShortcutStoryID, &row.ExternalTriggerID, &row.PipelineStage,
	)
	if err != nil {
		return nil, err
	}
	var tags []string
	_ = json.Unmarshal([]byte(row.Tags), &tags)
	reason := ""
	var createdAt time.Time
	_ = s.deps.DB.QueryRow(`SELECT reason, created_at FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&reason, &createdAt)
	prs := s.checkpointPRs(clawID)
	return &checkpointManifest{
		Schema:       1,
		CheckpointID: checkpointID,
		ClawID:       clawID,
		CreatedAt:    createdAt,
		Reason:       reason,
		Hub: checkpointHubManifest{
			Version:           s.deps.HubVersion(),
			Template:          row.Template,
			TemplateFilesSHA:  shaBytes([]byte(row.TemplateFiles)),
			DefaultModel:      row.DefaultModel,
			Tags:              tags,
			Color:             row.Color,
			FactoryName:       row.FactoryName,
			ConcurrencyGroup:  row.ConcurrencyGroup,
			LinearIssueID:     row.LinearIssueID,
			GitHubIssueID:     row.GitHubIssueID,
			ShortcutStoryID:   row.ShortcutStoryID,
			ExternalTriggerID: row.ExternalTriggerID,
			PipelineStage:     row.PipelineStage,
		},
		Provider:  checkpointProvider{Name: row.Provider, ProviderID: row.ProviderID},
		Messages:  checkpointMessages{BlobSHA256: msgSHA, Count: msgCount, CutoffAt: cutoff},
		Workspace: checkpointWorkspace{TreeSHA256: rootSHA},
		PRs:       prs,
		Files:     files,
	}, nil
}

func (s *Service) checkpointPRs(clawID string) []checkpointPR {
	rows, err := s.deps.DB.Query(`SELECT repo, pr_number, pr_url, last_ci_sha FROM claw_prs WHERE claw_id=? ORDER BY created_at ASC`, clawID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var prs []checkpointPR
	for rows.Next() {
		var pr checkpointPR
		if err := rows.Scan(&pr.Repo, &pr.Number, &pr.URL, &pr.LastCISHA); err == nil {
			prs = append(prs, pr)
		}
	}
	return prs
}

func (s *Service) pendingRestoreCheckpoint(clawID string) string {
	var checkpointID string
	_ = s.deps.DB.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id=?`, clawID).Scan(&checkpointID)
	return checkpointID
}

func (s *Service) markRestoreApplied(clawID, checkpointID string) {
	if checkpointID == "" {
		return
	}
	_, _ = s.deps.DB.Exec(`UPDATE claws SET restore_checkpoint_id='', restored_from_checkpoint_id=? WHERE id=?`, checkpointID, clawID)
}

func (s *Service) restoreCheckpointFiles(checkpointID string) ([]types.CheckpointFile, error) {
	if checkpointID == "" {
		return nil, nil
	}
	var rootSHA string
	if err := s.deps.DB.QueryRow(`SELECT root_tree_sha256 FROM claw_checkpoints WHERE id=? AND status='ready'`, checkpointID).Scan(&rootSHA); err != nil {
		return nil, err
	}
	return s.filesForTree(rootSHA)
}

func restoreRemotePath(path, home, workspace string) string {
	switch {
	case strings.HasPrefix(path, "workspace/"):
		return strings.TrimRight(workspace, "/") + "/" + strings.TrimPrefix(path, "workspace/")
	case path == "workspace":
		return workspace
	case strings.HasPrefix(path, ".openclaw/"):
		return strings.TrimRight(home, "/") + "/" + path
	default:
		return strings.TrimRight(workspace, "/") + "/" + path
	}
}

func (s *Service) RestoreCheckpointToDaytona(ctx context.Context, clawID, instanceID string, p *daytonaProvider.Provider) error {
	checkpointID := s.pendingRestoreCheckpoint(clawID)
	if checkpointID == "" {
		return nil
	}
	s.setBootstrapStatus(clawID, "Restoring checkpoint files")
	files, err := s.restoreCheckpointFiles(checkpointID)
	if err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(checkpointBlobPath(f.SHA256))
		if err != nil {
			return fmt.Errorf("read checkpoint blob %s: %w", f.SHA256, err)
		}
		remote := restoreRemotePath(f.Path, "/home/daytona", "/home/daytona/.openclaw/workspace")
		cmd := checkpointDaytonaRestoreCommand(remote, data)
		result, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", cmd}, 30*time.Second)
		if err != nil {
			return fmt.Errorf("restore %s: %w", f.Path, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("restore %s failed: %s", f.Path, result.Stdout)
		}
	}
	s.markRestoreApplied(clawID, checkpointID)
	return nil
}

func checkpointDaytonaRestoreCommand(remote string, data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf(`export HOME=/home/daytona; mkdir -p %s; base64 -d > %s <<'ELASTICCLAW_CHECKPOINT_EOF'
%s
ELASTICCLAW_CHECKPOINT_EOF`, checkpointShellQuote(filepath.Dir(remote)), checkpointShellQuote(remote), encoded)
}

func (s *Service) RestoreCheckpointToExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider) error {
	checkpointID := s.pendingRestoreCheckpoint(clawID)
	if checkpointID == "" {
		return nil
	}
	s.setBootstrapStatus(clawID, "Restoring checkpoint files")
	files, err := s.restoreCheckpointFiles(checkpointID)
	if err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(checkpointBlobPath(f.SHA256))
		if err != nil {
			return fmt.Errorf("read checkpoint blob %s: %w", f.SHA256, err)
		}
		remote := restoreRemotePath(f.Path, ".", ".openclaw/workspace")
		if err := p.WriteFile(ctx, vmName, remote, data); err != nil {
			return fmt.Errorf("restore %s: %w", f.Path, err)
		}
	}
	s.markRestoreApplied(clawID, checkpointID)
	return nil
}

func (s *Service) RestoreCheckpointToSSH(clawID, user, host string) error {
	checkpointID := s.pendingRestoreCheckpoint(clawID)
	if checkpointID == "" {
		return nil
	}
	s.setBootstrapStatus(clawID, "Restoring checkpoint files")
	files, err := s.restoreCheckpointFiles(checkpointID)
	if err != nil {
		return err
	}
	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.deps.SSHSigner())},
		HostKeyCallback: s.sshHostKeyCallback(host),
		Timeout:         30 * time.Second,
	}
	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()
	for _, f := range files {
		data, err := os.ReadFile(checkpointBlobPath(f.SHA256))
		if err != nil {
			return fmt.Errorf("read checkpoint blob %s: %w", f.SHA256, err)
		}
		remote := restoreRemotePath(f.Path, ".", ".openclaw/workspace")
		if err := sshWriteBytes(client, remote, data); err != nil {
			return fmt.Errorf("restore %s: %w", f.Path, err)
		}
	}
	s.markRestoreApplied(clawID, checkpointID)
	return nil
}

func sshWriteBytes(client *gossh.Client, remote string, data []byte) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s", checkpointShellQuote(filepath.Dir(remote)), checkpointShellQuote(remote))
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func checkpointShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func (s *Service) failCheckpoint(checkpointID, msg string) error {
	_, err := s.deps.DB.Exec(`UPDATE claw_checkpoints SET status='failed', error=?, completed_at=? WHERE id=?`, msg, now(), checkpointID)
	return err
}

func (s *Service) notifyCheckpointWaiter(checkpointID string, err error) {
	s.deps.CheckpointMu.Lock()
	ch := s.deps.CheckpointWaiters()[checkpointID]
	delete(s.deps.CheckpointWaiters(), checkpointID)
	s.deps.CheckpointMu.Unlock()
	if ch != nil {
		select {
		case ch <- err:
		default:
		}
	}
}

func (s *Service) finishCheckpointRequest(clawID, checkpointID string) {
	cc := s.deps.ClawConn(clawID)
	if cc != nil {
		cc.Lock()
		cc.SetCheckpointInProgressLocked(false)
		cc.Unlock()
	}
}

func validSHA256(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func shaBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ShortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

func checkpointRepoCount(prs []checkpointPR) int {
	seen := map[string]bool{}
	for _, pr := range prs {
		seen[pr.Repo] = true
	}
	return len(seen)
}
