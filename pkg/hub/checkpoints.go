package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	daytonaProvider "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	exedevProvider "github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"nhooyr.io/websocket/wsjson"
)

const (
	checkpointIdleInterval       = 10 * time.Minute
	checkpointMinInterval        = 5 * time.Minute
	checkpointRequestTimeout     = 2 * time.Minute
	checkpointTerminationTimeout = 90 * time.Second
	maxCheckpointBlobBytes       = 256 << 20

	defaultCheckpointRestoreParallelism = 8
	checkpointRestoreFileTimeout        = 2 * time.Minute
	checkpointRestoreProgressInterval   = 10 * time.Second
)

type checkpointFileWriter func(ctx context.Context, remotePath string, data []byte) error

var checkpointRestoreNow = time.Now

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

func (s *Server) checkpointScheduler() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.requestIdleCheckpoints()
	}
}

func (s *Server) requestIdleCheckpoints() {
	now := time.Now()
	s.mu.RLock()
	items := make([]struct {
		id string
		cc *clawConn
	}, 0, len(s.claws))
	for id, cc := range s.claws {
		items = append(items, struct {
			id string
			cc *clawConn
		}{id: id, cc: cc})
	}
	s.mu.RUnlock()

	for _, item := range items {
		item.cc.mu.RLock()
		lastUser := item.cc.lastUserMessageAt
		streaming := !item.cc.streamingStartedAt.IsZero() || item.cc.streamingMsgID != ""
		inProgress := item.cc.checkpointInProgress
		item.cc.mu.RUnlock()
		if streaming || inProgress || now.Sub(lastUser) < checkpointIdleInterval {
			continue
		}
		if s.hasRecentCheckpoint(item.id, checkpointMinInterval) {
			continue
		}
		go func(clawID string) {
			if _, err := s.requestCheckpoint(context.Background(), clawID, "idle-timer", "hub", false, checkpointRequestTimeout); err != nil {
				log.Printf("[checkpoint] idle request for %s failed: %v", shortID(clawID), err)
			}
		}(item.id)
	}
}

func (s *Server) hasRecentCheckpoint(clawID string, minAge time.Duration) bool {
	var completedAt time.Time
	err := s.db.QueryRow(`SELECT completed_at FROM claw_checkpoints WHERE claw_id=? AND status='ready' ORDER BY completed_at DESC LIMIT 1`, clawID).Scan(&completedAt)
	return err == nil && time.Since(completedAt) < minAge
}

func (s *Server) hasRecentCheckpointReason(clawID, reason string, minAge time.Duration) bool {
	var createdAt time.Time
	err := s.db.QueryRow(`SELECT created_at FROM claw_checkpoints WHERE claw_id=? AND reason=? AND status IN ('creating','ready') ORDER BY created_at DESC LIMIT 1`, clawID, reason).Scan(&createdAt)
	return err == nil && time.Since(createdAt) < minAge
}

func (s *Server) requestBootstrapCheckpoint(clawID string) {
	if s.hasRecentCheckpointReason(clawID, "bootstrap", time.Hour) {
		return
	}
	if _, err := s.requestCheckpoint(context.Background(), clawID, "bootstrap", "hub", false, checkpointRequestTimeout); err != nil {
		log.Printf("[checkpoint] bootstrap request for %s failed: %v", shortID(clawID), err)
	}
}

func (s *Server) handleClawCheckpoints(w http.ResponseWriter, r *http.Request, clawID string) {
	tenantID := tenantFromCtx(r)
	if r.Method == http.MethodPost && githubLoginFromContext(r.Context()) != "" {
		var tagsJSON string
		if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&tagsJSON); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var tags []string
		_ = json.Unmarshal([]byte(tagsJSON), &tags)
		s.mu.RLock()
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
		if !canModifyClaw(accessCfg, githubLoginFromContext(r.Context()), tags) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/claws/"), "/")
	if len(parts) == 4 && parts[1] == "checkpoints" && parts[3] == "restore" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.restoreClawFromCheckpoint(r.Context(), tenantID, clawID, parts[2]); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		jsonOK(w, map[string]string{"status": "restoring", "checkpoint_id": parts[2]})
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.db.Query(`SELECT id, claw_id, status, reason, created_by, manifest_sha256, root_tree_sha256, message_tree_sha256, workspace_tree_sha256, message_count, pr_count, repo_count, error, created_at, completed_at FROM claw_checkpoints WHERE tenant_id=? AND claw_id=? ORDER BY created_at DESC`, tenantID, clawID)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
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
		jsonOK(w, out)
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
		id, err := s.requestCheckpoint(r.Context(), clawID, reason, "user", false, checkpointRequestTimeout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		jsonOK(w, map[string]string{"id": id, "status": "creating"})
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) restoreClawFromCheckpoint(ctx context.Context, tenantID, clawID, checkpointID string) error {
	var status, manifestPath string
	if err := s.db.QueryRow(`SELECT status, manifest_path FROM claw_checkpoints WHERE id=? AND tenant_id=? AND claw_id=?`, checkpointID, tenantID, clawID).Scan(&status, &manifestPath); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("checkpoint not found")
		}
		return err
	}
	if status != "ready" {
		return fmt.Errorf("checkpoint is not ready")
	}
	if manifestPath == "" {
		return fmt.Errorf("checkpoint has no manifest")
	}

	// Preserve the current state if the bridge is still reachable.
	s.checkpointBeforeTermination(clawID, "pre-reset")

	var provider, providerID string
	if err := s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&provider, &providerID); err != nil {
		return err
	}
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "restoring checkpoint")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
	if providerID != "" {
		go s.terminateVM(provider, providerID)
	}

	_, err := s.db.Exec(`UPDATE claws SET status='provisioning', bootstrap_ok=0, bootstrap_status='Restoring checkpoint', provider_id='', restore_checkpoint_id=?, restored_from_checkpoint_id=? WHERE id=? AND tenant_id=?`,
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

type storedClawProvision struct {
	tenantID, name, template, provider, defaultModel, linearWorkspace, llmKey string
	nixEnabled, dockerEnabled                                                 int
	templateFiles                                                             map[string]string
	tmplCfg                                                                   *types.TemplateConfig
	env, resolvedSecrets                                                      map[string]string
}

// loadStoredClawProvision rebuilds the persisted claw configuration required to
// reprovision a claw, including the workspace/workflow-derived environment.
func (s *Server) loadStoredClawProvision(clawID string) (*storedClawProvision, error) {
	var (
		tenantID, name, template, provider, defaultModel, templateFilesJSON, githubReposJSON, linearWorkspace, llmKey, tagsJSON string
		nixEnabled, dockerEnabled                                                                                               int
	)
	err := s.db.QueryRow(
		`SELECT tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, llm_key, COALESCE(tags,'[]') FROM claws WHERE id=?`,
		clawID,
	).Scan(&tenantID, &name, &template, &provider, &defaultModel, &templateFilesJSON, &githubReposJSON, &linearWorkspace, &nixEnabled, &dockerEnabled, &llmKey, &tagsJSON)
	if err != nil {
		return nil, err
	}
	var templateFiles map[string]string
	if err := json.Unmarshal([]byte(templateFilesJSON), &templateFiles); err != nil {
		// Reprovisioning with no template files at all would produce a silently
		// crippled claw; fail the restore so the operator sees it instead.
		return nil, fmt.Errorf("unmarshal template_files: %w", err)
	}
	// A nil map is not an error: a claw stored without template files marshals
	// to "null", which unmarshals successfully into nil.
	if templateFiles == nil {
		templateFiles = map[string]string{}
	}
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		parsed, parseErr := config.ParseTemplateConfig([]byte(cfgContent))
		if parseErr != nil {
			log.Printf("[restore] claw %s: failed to parse elasticclaw-config.yaml; reprovisioning without template config: %v", shortID(clawID), parseErr)
		} else {
			tmplCfg = parsed
		}
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	_, workflowName := workflowTags(tags)
	workspaceName := template
	var workflow *types.WorkflowConfig
	if workflowName != "" {
		if _, loadedWorkflow, found := loadWorkflowPipelineContext(workspaceName, workflowName); found {
			workflow = loadedWorkflow
		} else {
			log.Printf("[restore] claw %s: workflow %q in workspace %q not found; reprovisioning without workflow secrets", shortID(clawID), workflowName, workspaceName)
		}
	}
	env, resolvedSecrets, resolveErr := s.resolveClawEnv(workspaceName, workflow, tmplCfg, clawID)
	if resolveErr != nil {
		return nil, resolveErr
	}
	return &storedClawProvision{
		tenantID: tenantID, name: name, template: template, provider: provider,
		defaultModel: defaultModel, linearWorkspace: linearWorkspace, llmKey: llmKey,
		nixEnabled: nixEnabled, dockerEnabled: dockerEnabled, templateFiles: templateFiles,
		tmplCfg: tmplCfg, env: env, resolvedSecrets: resolvedSecrets,
	}, nil
}

func (s *Server) provisionStoredClaw(clawID string) {
	stored, err := s.loadStoredClawProvision(clawID)
	if err != nil {
		log.Printf("[restore] failed to load claw %s: %v", shortID(clawID), err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore failed: %v", err), false)
		return
	}
	s.mu.RLock()
	provCfg, ok := s.hubCfg.Providers[stored.provider]
	s.mu.RUnlock()
	if !ok {
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore failed: provider %q is not configured", stored.provider), false)
		return
	}
	stored.templateFiles["SECRETS.md"] = buildSecretsFile(stored.resolvedSecrets)
	stored.templateFiles = injectFigmaAPIDocs(stored.templateFiles, stored.env)
	fileBytes := make(map[string][]byte, len(stored.templateFiles))
	for k, v := range stored.templateFiles {
		fileBytes[k] = []byte(v)
	}
	req := types.CreateClawRequest{
		Name:         stored.name,
		TemplateName: stored.template,
		Provider:     stored.provider,
		DefaultModel: stored.defaultModel,
		Files:        stored.templateFiles,
		Env:          stored.env,
		Nix:          stored.nixEnabled != 0,
		Docker:       stored.dockerEnabled != 0,
		LLMKey:       stored.llmKey,
		ProviderName: "ec-" + clawID[:8] + "-r" + uuid.New().String()[:4],
	}
	if stored.tmplCfg != nil {
		req.InstanceType = stored.tmplCfg.InstanceType
		req.Snapshot = stored.tmplCfg.Snapshot
		req.TTL = stored.tmplCfg.TTL
	}
	ctx := context.Background()
	var provErr error
	switch stored.provider {
	case "daytona":
		provErr = s.provisionDaytona(ctx, clawID, req, provCfg, fileBytes, stored.env)
	case "replicated":
		provErr = s.provisionReplicated(ctx, clawID, req, provCfg, stored.env)
	case "exedev":
		provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, stored.env)
	case "lambda-microvms":
		provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, fileBytes)
	default:
		provErr = fmt.Errorf("unsupported provider: %s", stored.provider)
	}
	if provErr != nil {
		log.Printf("[restore] provision failed for claw %s: %v", clawID, provErr)
		s.recordPipelineFailureRecord(clawID, "provision", "provision", "FATAL", provErr.Error(), map[string]interface{}{"error.type": "provision_failed"})
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore provision failed: %v", provErr), false)
		return
	}
	var restoreCheckpointID string
	_ = s.db.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id=?`, clawID).Scan(&restoreCheckpointID)
	createdAt := now()
	_, _ = s.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,format,delivered_at) VALUES(?,?,?,?,?,?,?,?)`,
		uuid.New().String(), clawID, stored.tenantID, "system", fmt.Sprintf("[hub] Restoring from checkpoint %s.", restoreCheckpointID), createdAt, "pre", createdAt)
}

func (s *Server) requestCheckpoint(ctx context.Context, clawID, reason, createdBy string, wait bool, timeout time.Duration) (string, error) {
	tenantID, provider, providerID, err := s.clawCheckpointIdentity(clawID)
	if err != nil {
		return "", err
	}

	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		checkpointID := uuid.New().String()
		if err := s.insertCheckpoint(checkpointID, tenantID, clawID, reason, createdBy, provider, providerID); err != nil {
			return "", err
		}
		if err := s.completeMetadataOnlyCheckpoint(checkpointID, clawID, reason, "bridge unreachable"); err != nil {
			return checkpointID, err
		}
		return checkpointID, nil
	}

	cc.mu.Lock()
	busy := !cc.streamingStartedAt.IsZero() || cc.streamingMsgID != ""
	if busy || cc.checkpointInProgress {
		if cc.pendingCheckpointID != "" {
			stronger := strongerCheckpointReason(cc.pendingCheckpointReason, reason)
			if stronger != cc.pendingCheckpointReason {
				cc.pendingCheckpointReason = stronger
				_, _ = s.db.Exec(`UPDATE claw_checkpoints SET reason=? WHERE id=?`, stronger, cc.pendingCheckpointID)
			}
			pendingID := cc.pendingCheckpointID
			cc.mu.Unlock()
			if wait {
				return pendingID, s.waitCheckpointStatus(ctx, pendingID, timeout)
			}
			return pendingID, nil
		}
		checkpointID := uuid.New().String()
		if err := s.insertCheckpoint(checkpointID, tenantID, clawID, reason, createdBy, provider, providerID); err != nil {
			cc.mu.Unlock()
			return "", err
		}
		cc.pendingCheckpointID = checkpointID
		cc.pendingCheckpointReason = reason
		cc.pendingCheckpointBy = createdBy
		cc.mu.Unlock()
		if wait {
			return checkpointID, s.waitCheckpointStatus(ctx, checkpointID, timeout)
		}
		return checkpointID, nil
	}
	checkpointID := uuid.New().String()
	if err := s.insertCheckpoint(checkpointID, tenantID, clawID, reason, createdBy, provider, providerID); err != nil {
		cc.mu.Unlock()
		return "", err
	}
	cc.checkpointInProgress = true
	cc.mu.Unlock()
	return s.dispatchCheckpoint(ctx, cc, clawID, checkpointID, reason, wait, timeout)
}

func (s *Server) waitCheckpointStatus(ctx context.Context, checkpointID string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status, msg string
		_ = s.db.QueryRow(`SELECT status, error FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&status, &msg)
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

func (s *Server) dispatchCheckpoint(ctx context.Context, cc *clawConn, clawID, checkpointID, reason string, wait bool, timeout time.Duration) (string, error) {
	ch := make(chan error, 1)
	s.checkpointMu.Lock()
	if s.checkpointWaiters == nil {
		s.checkpointWaiters = make(map[string]chan error)
	}
	s.checkpointWaiters[checkpointID] = ch
	s.checkpointMu.Unlock()

	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	payload := types.CheckpointCreatePayload{
		CheckpointID: checkpointID,
		Reason:       reason,
		HubURL:       s.clawHubURL(),
		ClawToken:    clawToken,
	}
	if err := wsjson.Write(ctx, cc.conn, types.WSMessage{Type: "checkpoint_create", Payload: payload}); err != nil {
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

func (s *Server) drainPendingCheckpoint(clawID string) {
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		return
	}
	cc.mu.Lock()
	checkpointID := cc.pendingCheckpointID
	reason := cc.pendingCheckpointReason
	cc.pendingCheckpointID = ""
	cc.pendingCheckpointReason = ""
	cc.pendingCheckpointBy = ""
	inProgress := cc.checkpointInProgress
	if reason != "" && !inProgress {
		cc.checkpointInProgress = true
	}
	cc.mu.Unlock()
	if checkpointID == "" || reason == "" || inProgress {
		return
	}
	go func() {
		if _, err := s.dispatchCheckpoint(context.Background(), cc, clawID, checkpointID, reason, false, checkpointRequestTimeout); err != nil {
			log.Printf("[checkpoint] queued request for %s failed: %v", shortID(clawID), err)
		}
	}()
}

func (s *Server) checkpointBeforeTermination(clawID, reason string) {
	if _, err := s.requestCheckpoint(context.Background(), clawID, "termination:"+reason, "hub", true, checkpointTerminationTimeout); err != nil {
		log.Printf("[checkpoint] termination checkpoint for %s failed: %v", shortID(clawID), err)
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

func (s *Server) clawCheckpointIdentity(clawID string) (tenantID, provider, providerID string, err error) {
	err = s.db.QueryRow(`SELECT tenant_id, COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &provider, &providerID)
	return tenantID, provider, providerID, err
}

func (s *Server) insertCheckpoint(id, tenantID, clawID, reason, createdBy, provider, providerID string) error {
	_, err := s.db.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, reason, created_by, provider, provider_id_at_create, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, tenantID, clawID, "creating", reason, createdBy, provider, providerID, now())
	return err
}

func (s *Server) handleCheckpointInternal(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/checkpoints/"), "/")
	if len(parts) != 2 {
		http.Error(w, "not found", http.StatusNotFound)
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
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var plan types.CheckpointPlan
		if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
			http.Error(w, "invalid plan", http.StatusBadRequest)
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
		jsonOK(w, types.CheckpointPlanAck{Upload: missing})
	case "complete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var complete types.CheckpointComplete
		if err := json.NewDecoder(r.Body).Decode(&complete); err != nil {
			http.Error(w, "invalid complete", http.StatusBadRequest)
			return
		}
		if complete.Error != "" {
			_ = s.failCheckpoint(checkpointID, complete.Error)
			s.notifyCheckpointWaiter(checkpointID, fmt.Errorf("%s", complete.Error))
			s.finishCheckpointRequest(clawID, checkpointID)
			s.drainPendingCheckpoint(clawID)
			jsonOK(w, map[string]string{"status": "failed"})
			return
		}
		if err := s.finalizeCheckpoint(checkpointID, tenantID, clawID, complete.RootSHA256); err != nil {
			_ = s.failCheckpoint(checkpointID, err.Error())
			s.notifyCheckpointWaiter(checkpointID, err)
			s.finishCheckpointRequest(clawID, checkpointID)
			s.drainPendingCheckpoint(clawID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.notifyCheckpointWaiter(checkpointID, nil)
		s.finishCheckpointRequest(clawID, checkpointID)
		s.drainPendingCheckpoint(clawID)
		jsonOK(w, map[string]string{"status": "ready"})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleCheckpointBlobUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sha := strings.TrimPrefix(r.URL.Path, "/api/checkpoints/blob/")
	if !validSHA256(sha) {
		http.Error(w, "bad sha", http.StatusBadRequest)
		return
	}
	token := r.Header.Get("X-Claw-Token")
	if token == "" {
		token = r.URL.Query().Get("claw_token")
	}
	if _, err := s.tenantByClawToken(token); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCheckpointBlobBytes)
	path := checkpointBlobPath(sha)
	if _, err := os.Stat(path); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	tmp := path + ".tmp-" + uuid.New().String()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), r.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		var maxBytesErr *http.MaxBytesError
		if errors.As(copyErr, &maxBytesErr) {
			http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		_ = os.Remove(tmp)
		http.Error(w, "sha mismatch", http.StatusBadRequest)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(path); statErr == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticateCheckpointClaw(w http.ResponseWriter, r *http.Request, checkpointID string) (tenantID, clawID string, ok bool) {
	token := r.Header.Get("X-Claw-Token")
	if token == "" {
		token = r.URL.Query().Get("claw_token")
	}
	resolvedTenant, err := s.tenantByClawToken(token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	err = s.db.QueryRow(`SELECT tenant_id, claw_id FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&tenantID, &clawID)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return "", "", false
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return "", "", false
	}
	if tenantID != resolvedTenant {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", false
	}
	return tenantID, clawID, true
}

func (s *Server) finalizeCheckpoint(checkpointID, tenantID, clawID, rootSHA string) error {
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
	_, err = s.db.Exec(`UPDATE claw_checkpoints SET status='ready', manifest_sha256=?, manifest_path=?, root_tree_sha256=?, message_tree_sha256=?, workspace_tree_sha256=?, message_count=?, pr_count=?, repo_count=?, completed_at=? WHERE id=?`,
		manifestSHA, path, rootSHA, msgSHA, rootSHA, msgCount, len(manifest.PRs), checkpointRepoCount(manifest.PRs), now(), checkpointID)
	return err
}

func (s *Server) completeMetadataOnlyCheckpoint(checkpointID, clawID, reason, detail string) error {
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
	_, err = s.db.Exec(`UPDATE claw_checkpoints SET status='ready', manifest_sha256=?, manifest_path=?, message_tree_sha256=?, message_count=?, error=?, completed_at=? WHERE id=?`,
		manifestSHA, path, msgSHA, msgCount, detail, now(), checkpointID)
	return err
}

func (s *Server) filesForTree(rootSHA string) ([]types.CheckpointFile, error) {
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

func (s *Server) writeMessageCheckpointBlob(clawID, tenantID string) (string, int, time.Time, error) {
	rows, err := s.db.Query(`SELECT id, role, content, format, created_at FROM messages WHERE claw_id=? AND tenant_id=? ORDER BY created_at ASC`, clawID, tenantID)
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

func (s *Server) buildCheckpointManifest(checkpointID, clawID, rootSHA, msgSHA string, msgCount int, cutoff time.Time, files []types.CheckpointFile) (*checkpointManifest, error) {
	var row struct {
		TenantID, Template, Provider, ProviderID, DefaultModel, TemplateFiles, Tags, Color                             string
		FactoryName, ConcurrencyGroup, LinearIssueID, GitHubIssueID, ShortcutStoryID, ExternalTriggerID, PipelineStage string
	}
	err := s.db.QueryRow(`SELECT tenant_id, template, provider, provider_id, default_model, template_files, tags, color, factory_name, concurrency_group, linear_issue_id, github_issue_id, shortcut_story_id, external_trigger_id, pipeline_stage FROM claws WHERE id=?`, clawID).Scan(
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
	_ = s.db.QueryRow(`SELECT reason, created_at FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&reason, &createdAt)
	prs := s.checkpointPRs(clawID)
	return &checkpointManifest{
		Schema:       1,
		CheckpointID: checkpointID,
		ClawID:       clawID,
		CreatedAt:    createdAt,
		Reason:       reason,
		Hub: checkpointHubManifest{
			Version:           Version,
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

func (s *Server) checkpointPRs(clawID string) []checkpointPR {
	// Resolved rows (merged/closed) survive in claw_prs until the claw is
	// finalized, but a checkpoint lists tracked WORK — restoring one taken
	// after a partial merge must not present already-merged PRs as open.
	rows, err := s.db.Query(`SELECT repo, pr_number, pr_url, last_ci_sha FROM claw_prs WHERE claw_id=? AND state NOT IN ('merged','closed') ORDER BY created_at ASC`, clawID)
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

func (s *Server) pendingRestoreCheckpoint(clawID string) string {
	var checkpointID string
	_ = s.db.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id=?`, clawID).Scan(&checkpointID)
	return checkpointID
}

func (s *Server) markRestoreApplied(clawID, checkpointID string) {
	if checkpointID == "" {
		return
	}
	_, _ = s.db.Exec(`UPDATE claws SET restore_checkpoint_id='', restored_from_checkpoint_id=? WHERE id=?`, checkpointID, clawID)
}

func (s *Server) restoreCheckpointFiles(checkpointID string) ([]types.CheckpointFile, error) {
	if checkpointID == "" {
		return nil, nil
	}
	var rootSHA string
	if err := s.db.QueryRow(`SELECT root_tree_sha256 FROM claw_checkpoints WHERE id=? AND status='ready'`, checkpointID).Scan(&rootSHA); err != nil {
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

func (s *Server) restoreCheckpointFilesTo(ctx context.Context, clawID, checkpointID string, files []types.CheckpointFile, remotePathFor func(path string) string, parallelism int, write checkpointFileWriter) error {
	totalBytes := int64(0)
	for _, f := range files {
		totalBytes += f.Size
	}
	total := len(files)
	log.Printf("[checkpoint] restoring %d files (%d bytes) into claw %s", total, totalBytes, clawID)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(parallelism)
	var done atomic.Int64
	var progressMu sync.Mutex
	lastProgress := checkpointRestoreNow()
	reportProgress := func() {
		progressMu.Lock()
		defer progressMu.Unlock()
		now := checkpointRestoreNow()
		if now.Sub(lastProgress) < checkpointRestoreProgressInterval {
			return
		}
		lastProgress = now
		completed := done.Load()
		log.Printf("[checkpoint] claw %s: restored %d/%d files", clawID, completed, total)
		s.setBootstrapStatus(clawID, fmt.Sprintf("Restoring checkpoint files (%d/%d)", completed, total))
	}

	for _, f := range files {
		f := f
		group.Go(func() error {
			// errgroup still runs every submitted worker after the first
			// failure. The SSH writer ignores its context, so without this
			// check a failed restore would keep uploading the whole manifest
			// before returning the error.
			if err := groupCtx.Err(); err != nil {
				return err
			}
			data, err := os.ReadFile(checkpointBlobPath(f.SHA256))
			if err != nil {
				return fmt.Errorf("read checkpoint blob %s: %w", f.SHA256, err)
			}
			writeCtx, cancel := context.WithTimeout(groupCtx, checkpointRestoreFileTimeout)
			err = write(writeCtx, remotePathFor(f.Path), data)
			cancel()
			if err != nil {
				return fmt.Errorf("restore %s: %w", f.Path, err)
			}
			done.Add(1)
			reportProgress()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	log.Printf("[checkpoint] claw %s: restore complete (%d files)", clawID, total)
	return nil
}

func (s *Server) restoreCheckpointToDaytona(ctx context.Context, clawID, instanceID string, p *daytonaProvider.Provider) error {
	checkpointID := s.pendingRestoreCheckpoint(clawID)
	if checkpointID == "" {
		return nil
	}
	s.setBootstrapStatus(clawID, "Restoring checkpoint files")
	files, err := s.restoreCheckpointFiles(checkpointID)
	if err != nil {
		return err
	}
	if err := s.restoreCheckpointFilesTo(ctx, clawID, checkpointID, files, func(path string) string {
		return restoreRemotePath(path, "/home/daytona", "/home/daytona/.openclaw/workspace")
	}, defaultCheckpointRestoreParallelism, func(ctx context.Context, remote string, data []byte) error {
		return p.WriteFile(ctx, instanceID, remote, data)
	}); err != nil {
		return err
	}
	s.markRestoreApplied(clawID, checkpointID)
	return nil
}

func (s *Server) restoreCheckpointToExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider) error {
	checkpointID := s.pendingRestoreCheckpoint(clawID)
	if checkpointID == "" {
		return nil
	}
	s.setBootstrapStatus(clawID, "Restoring checkpoint files")
	files, err := s.restoreCheckpointFiles(checkpointID)
	if err != nil {
		return err
	}
	if err := s.restoreCheckpointFilesTo(ctx, clawID, checkpointID, files, func(path string) string {
		return restoreRemotePath(path, ".", ".openclaw/workspace")
	}, 1, func(ctx context.Context, remote string, data []byte) error {
		return p.WriteFile(ctx, vmName, remote, data)
	}); err != nil {
		return err
	}
	s.markRestoreApplied(clawID, checkpointID)
	return nil
}

func (s *Server) restoreCheckpointToSSH(clawID, user, host string) error {
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
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: s.sshHostKeyCallback(host),
		Timeout:         30 * time.Second,
	}
	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()
	if err := s.restoreCheckpointFilesTo(context.Background(), clawID, checkpointID, files, func(path string) string {
		return restoreRemotePath(path, ".", ".openclaw/workspace")
	}, 1, func(_ context.Context, remote string, data []byte) error {
		return sshWriteBytes(client, remote, data)
	}); err != nil {
		return err
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

func (s *Server) failCheckpoint(checkpointID, msg string) error {
	_, err := s.db.Exec(`UPDATE claw_checkpoints SET status='failed', error=?, completed_at=? WHERE id=?`, msg, now(), checkpointID)
	return err
}

func (s *Server) notifyCheckpointWaiter(checkpointID string, err error) {
	s.checkpointMu.Lock()
	ch := s.checkpointWaiters[checkpointID]
	delete(s.checkpointWaiters, checkpointID)
	s.checkpointMu.Unlock()
	if ch != nil {
		select {
		case ch <- err:
		default:
		}
	}
}

func (s *Server) finishCheckpointRequest(clawID, checkpointID string) {
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc != nil {
		cc.mu.Lock()
		cc.checkpointInProgress = false
		cc.mu.Unlock()
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

func shortID(id string) string {
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
