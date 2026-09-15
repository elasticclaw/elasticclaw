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

// checkpointManifestSchema is the manifest format version.
//
// Schema 1 inlined the complete file list in Files. Nothing ever read it back
// — restore resolves the file list from the root tree blob via filesForTree —
// so the field was pure duplication of content-addressed data, at roughly
// 700 KB per manifest. Schema 2 keeps only the aggregates.
const checkpointManifestSchema = 2

type checkpointManifest struct {
	Schema       int                   `json:"schema"`
	CheckpointID string                `json:"checkpoint_id"`
	ClawID       string                `json:"claw_id"`
	CreatedAt    time.Time             `json:"created_at"`
	Reason       string                `json:"reason"`
	Hub          checkpointHubManifest `json:"hub"`
	Provider     checkpointProvider    `json:"provider"`
	Messages     checkpointMessages    `json:"messages"`
	Workspace    checkpointWorkspace   `json:"workspace"`
	PRs          []checkpointPR        `json:"prs"`

	// FilesCount and FilesBytes summarise the workspace captured by this
	// checkpoint. The list itself lives in the root tree blob addressed by
	// Workspace.TreeSHA256, deduplicated across every checkpoint sharing it.
	FilesCount int   `json:"files_count"`
	FilesBytes int64 `json:"files_bytes"`
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

// writeFileAtomic writes via a temporary file and a rename, so a reader never
// observes a partial file.
//
// A bare os.WriteFile leaves truncated content behind when the disk fills, and
// a truncated manifest is not merely one broken checkpoint: the blob sweep
// derives its keep set by parsing every manifest, so one unparseable file used
// to switch reclamation off entirely. That made disk-full -- the exact
// condition this feature exists to relieve -- self-sustaining. Blob uploads
// already used tmp+rename; manifests and the message blob now match them.
//
// The temporary file carries the ".tmp-" marker the blob sweep already skips,
// so a crash between create and rename leaves something the sweep ignores
// rather than something it mistakes for an orphan.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp-" + uuid.New().String()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Sync before the rename: without it the rename can reach the disk ahead of
	// the contents, and a power loss then publishes an empty file under a name
	// that claims to be complete.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Server) checkpointScheduler() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.requestIdleCheckpoints()
		s.failStuckCreatingCheckpoints()
	}
}

// checkpointCreatingMaxAge bounds how long a checkpoint may stay 'creating'.
//
// Generous on purpose: the legitimate upper bound is one claw's full upload
// phase, which is minutes on a large workspace and can be much worse on a slow
// link. Anything past this is a claw that died mid-upload, and until the row is
// failed it goes on holding every digest it planned -- forever, on a hub whose
// reaper is disabled, since the only other release is reconcileOnBoot.
const checkpointCreatingMaxAge = 6 * time.Hour

// failStuckCreatingCheckpoints fails 'creating' rows older than the bound and
// releases the blobs they were holding.
//
// Both halves probe with a read before writing. This runs every minute, and
// under _txlock=immediate even an UPDATE or DELETE that matches nothing takes
// the hub's one write lock; the steady state for both is "nothing to do", so the
// probes are what keep this off the write path entirely. The probe used to sit
// after an unconditional UPDATE, which meant it never achieved the thing it was
// written for.
func (s *Server) failStuckCreatingCheckpoints() {
	cutoff := now().Add(-checkpointCreatingMaxAge)
	var stuck bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM claw_checkpoints WHERE status='creating' AND created_at < ?)`, cutoff).
		Scan(&stuck); err != nil {
		log.Printf("[checkpoint] probe stuck creating checkpoints: %v", err)
		return
	}
	if !stuck {
		return
	}
	if count, err := failStuckCreatingCheckpointsTx(s.db, cutoff); err != nil {
		log.Printf("[checkpoint] fail stuck creating checkpoints: %v", err)
	} else if count > 0 {
		log.Printf("[checkpoint] failed %d checkpoint(s) stuck in 'creating' for over %s", count, checkpointCreatingMaxAge)
	}
}

// failStuckCreatingCheckpointsTx fails the abandoned rows and drops their
// references together, so a checkpoint is never recorded as failed while still
// holding blobs (nor the reverse).
func failStuckCreatingCheckpointsTx(db *sql.DB, cutoff time.Time) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM checkpoint_blob_refs WHERE checkpoint_id IN (
		SELECT id FROM claw_checkpoints WHERE status='creating' AND created_at < ?)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.Exec(
		`UPDATE claw_checkpoints SET status='failed', error='checkpoint abandoned while creating', completed_at=?
		  WHERE status='creating' AND created_at < ?`, now(), cutoff)
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// releaseOrphanedCheckpointBlobRefs drops every reference whose checkpoint row
// no longer exists at all.
//
// A reference outlives the plan by design now, so "the row is not 'creating'
// any more" is no longer evidence of anything. The only shape that is still
// certainly garbage is an edge pointing at a checkpoint that was deleted --
// which a crash between the row delete and the edge delete can leave behind, and
// which would otherwise pin those blobs for the life of the database.
//
// It runs only on boot (reconcileCheckpointsOnBoot), the one place that can
// follow such a crash. It used to run on every scheduler tick, where its
// NOT-EXISTS probe was a full scan of the edge table once a minute, and after a
// failed expiry delete -- but every path that deletes a row deletes its edges
// in the same transaction, and a failed delete rolls both back, so neither
// place could ever have found anything.
func releaseOrphanedCheckpointBlobRefs(db *sql.DB) (int64, error) {
	var orphaned bool
	if err := db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM checkpoint_blob_refs r
		 WHERE NOT EXISTS (SELECT 1 FROM claw_checkpoints c WHERE c.id = r.checkpoint_id))`).
		Scan(&orphaned); err != nil {
		return 0, err
	}
	if !orphaned {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM checkpoint_blob_refs
		 WHERE checkpoint_id NOT IN (SELECT id FROM claw_checkpoints)`)
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	return count, nil
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
	err := s.db.QueryRow(`SELECT completed_at FROM claw_checkpoints WHERE claw_id=? AND status IN ('ready','skipped') ORDER BY completed_at DESC LIMIT 1`, clawID).Scan(&completedAt)
	return err == nil && time.Since(completedAt) < minAge
}

func (s *Server) hasRecentCheckpointReason(clawID, reason string, minAge time.Duration) bool {
	var createdAt time.Time
	err := s.db.QueryRow(`SELECT created_at FROM claw_checkpoints WHERE claw_id=? AND reason=? AND status IN ('creating','ready') ORDER BY created_at DESC LIMIT 1`, clawID, reason).Scan(&createdAt)
	return err == nil && time.Since(createdAt) < minAge
}

// checkpointDuplicatesPrevious reports whether an idle checkpoint captured the
// same workspace tree as the claw's last recorded checkpoint. Only idle-timer
// checkpoints are eligible: a checkpoint taken at a lifecycle boundary is worth
// keeping even when nothing changed on disk, because it marks the transition.
func (s *Server) checkpointDuplicatesPrevious(checkpointID, clawID, rootSHA string) bool {
	if rootSHA == "" {
		return false
	}
	var reason string
	if err := s.db.QueryRow(`SELECT reason FROM claw_checkpoints WHERE id=?`, checkpointID).Scan(&reason); err != nil || reason != "idle-timer" {
		return false
	}
	var prev string
	err := s.db.QueryRow(
		`SELECT root_tree_sha256 FROM claw_checkpoints
		  WHERE claw_id=? AND id<>? AND status IN ('ready','skipped') AND root_tree_sha256<>''
		  ORDER BY created_at DESC LIMIT 1`, clawID, checkpointID).Scan(&prev)
	return err == nil && prev == rootSHA
}

// markCheckpointSkipped records the checkpoint without a manifest. The row is
// kept so the timeline still shows the claw was idle at that moment.
//
// The plan-time reference edges are KEPT, and the root tree gets one of its own.
// A skipped row names the tree of the ready checkpoint it duplicated, so it is a
// genuine holder of those blobs for as long as it exists; the ready row holds
// the same ones, and the blob only becomes collectable when BOTH have let go.
// Compaction is what releases them, on a claw that is finished.
func (s *Server) markCheckpointSkipped(checkpointID, rootSHA string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`UPDATE claw_checkpoints SET status='skipped', root_tree_sha256=?, workspace_tree_sha256=?, completed_at=? WHERE id=? AND status='creating'`,
		rootSHA, rootSHA, now(), checkpointID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return errCheckpointNotCreating
	}
	if err := addCheckpointBlobRefsTx(tx, checkpointID, []string{rootSHA}); err != nil {
		return err
	}
	return tx.Commit()
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

// recordCheckpointBlobRefs records what the checkpoint plans to hold, before a
// single byte is uploaded: one edge from the checkpoint to its root tree, plus
// -- when the hub has never seen that tree -- the expansion of the tree into
// the file digests it lists.
//
// These are not claims with an expiry: they are the reference. They are written
// here because the plan is the first moment the hub knows which blobs this
// checkpoint will hold, and they survive finalize -- from then on they are the
// only thing that says the published checkpoint holds them. Nothing reconstructs
// that from manifests or tree blobs any more.
//
// The expansion is committed HERE, in the same transaction as the root edge,
// and not when the tree blob is uploaded. The bridge sends the complete file
// list and the tree digest in the plan, so the hub has everything it needs. If
// the expansion were written later, every file blob uploaded between the plan
// and the tree upload would sit with no edge covering it, protected only by
// the sweeper's mtime grace -- which is exactly the mark-and-sweep reasoning the
// reference model exists to remove. Writing it at plan time is what lets the
// plan/sweep interlock (planBlobMissing / removeUnclaimedBlob) carry over
// unchanged: every digest the plan answers for is referenced before the answer
// is sent.
//
// Recording before answering is what makes the plan's dedup answer safe. The
// handler tells the claw to skip uploading a blob the hub already has, which
// means that blob's mtime stays whatever it was when some other claw wrote it --
// possibly months ago, well outside any grace window. Every planned digest is
// recorded, not only the ones the claw must upload: the reused ones are
// precisely the ones at risk.
//
// A plan without a usable root digest falls back to one edge per file under
// the checkpoint itself. The bridge always sends the root, so this is a rule
// for a caller the hub does not have yet, not a path it expects to take -- but
// the safety property must not depend on what the bridge sends.
func (s *Server) recordCheckpointBlobRefs(checkpointID, rootSHA string, files []types.CheckpointFile) error {
	root := normalizeBlobDigest(rootSHA)
	if len(files) == 0 && root == "" {
		// Nothing to reference, so there is no reason to take the write lock --
		// but the status still has to be checked, because it is what the plan
		// handler turns into its answer.
		return s.requireCheckpointCreating(s.db.QueryRow(`SELECT status FROM claw_checkpoints WHERE id=?`, checkpointID))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A plan may only add edges while the row is still under construction. Once
	// the row has gone terminal its edge set is settled: a 'failed' or expired
	// row released everything it held, and re-adding an edge under it would pin
	// blobs nothing can ever reach. A delayed or retried plan lands here, and
	// answering it would tell the claw to skip uploads for a checkpoint that no
	// longer exists. Reject it instead, so the caller can fail the plan loudly.
	if err := s.requireCheckpointCreating(tx.QueryRow(`SELECT status FROM claw_checkpoints WHERE id=?`, checkpointID)); err != nil {
		return err
	}
	if root == "" {
		digests := make([]string, 0, len(files))
		for _, f := range files {
			digests = append(digests, f.SHA256)
		}
		if err := addCheckpointBlobRefsTx(tx, checkpointID, digests); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := addCheckpointBlobRefsTx(tx, checkpointID, []string{root}); err != nil {
		return err
	}
	if err := addTreeBlobRefsTx(tx, root, files); err != nil {
		return err
	}
	return tx.Commit()
}

// addCheckpointBlobRefsTx inserts reference edges inside a caller's transaction,
// so publishing a checkpoint and recording what it holds cannot come apart.
// Non-digest values are skipped: an empty root tree (a metadata-only capture)
// and a malformed digest both reference nothing.
func addCheckpointBlobRefsTx(tx *sql.Tx, checkpointID string, digests []string) error {
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO checkpoint_blob_refs(checkpoint_id, sha256) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sha := range digests {
		clean := normalizeBlobDigest(sha)
		if clean == "" {
			continue
		}
		if _, err := stmt.Exec(checkpointID, clean); err != nil {
			return err
		}
	}
	return nil
}

// addTreeBlobRefsTx records the expansion of one workspace tree into the file
// digests it lists, once per distinct tree, inside the caller's transaction.
//
// The probe first. On the production hub 96% of idle checkpoints capture a tree
// the hub has already seen, and for those the whole cost of this call is one
// indexed EXISTS on the tree digest; only a genuinely new tree pays one insert
// per file. The probe is safe to trust because an expansion is written in one
// transaction and garbage-collected one whole tree per statement
// (pruneUnreferencedTreeBlobRefs), so a tree with any row has all its rows.
//
// The tree's own digest is not listed under itself: the bridge puts the tree
// blob in the plan's file list, and a self-edge is noise the keep set does not
// need -- the checkpoint edge already names the tree.
func addTreeBlobRefsTx(tx *sql.Tx, treeSHA string, files []types.CheckpointFile) error {
	tree := normalizeBlobDigest(treeSHA)
	if tree == "" || len(files) == 0 {
		return nil
	}
	var known bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tree_blob_refs WHERE tree_sha256=?)`, tree).Scan(&known); err != nil {
		return err
	}
	if known {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO tree_blob_refs(tree_sha256, sha256) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range files {
		clean := normalizeBlobDigest(f.SHA256)
		if clean == "" || clean == tree {
			continue
		}
		if _, err := stmt.Exec(tree, clean); err != nil {
			return err
		}
	}
	return nil
}

// errCheckpointNotCreating reports a transition attempted against a checkpoint
// row that is no longer under construction: a stale plan retry, or a 'complete'
// arriving after the row already went terminal. Both are ignorable at the call
// site, and both must NOT be allowed to mutate the row.
var errCheckpointNotCreating = errors.New("checkpoint is no longer creating")

// requireCheckpointCreating turns a status lookup into the guard. A row that
// vanished is treated the same as a terminal one: expiry deletes rows of every
// status, including 'creating', so a plan can outlive its own checkpoint.
func (s *Server) requireCheckpointCreating(row *sql.Row) error {
	var status string
	if err := row.Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return errCheckpointNotCreating
		}
		return err
	}
	if status != "creating" {
		return errCheckpointNotCreating
	}
	return nil
}

// deleteCheckpointBlobRefsTx releases every blob this checkpoint holds, inside
// the caller's transaction.
//
// It belongs in the SAME transaction as the status change that justifies it --
// fail, expiry, compaction. That is the whole point of the model: a compacted
// row releases its blobs by construction rather than by some later pass
// remembering to clear the right digest columns, and a release that commits
// while the status change rolls back would leave a live checkpoint holding
// nothing.
func deleteCheckpointBlobRefsTx(tx *sql.Tx, checkpointID string) error {
	_, err := tx.Exec(`DELETE FROM checkpoint_blob_refs WHERE checkpoint_id=?`, checkpointID)
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
		// Record the reference edges BEFORE answering. The answer is what lets
		// the claw skip re-uploading blobs the hub already holds, so from the
		// moment it is sent the checkpoint depends on files nothing else keeps
		// alive. Failing to record them must fail the plan rather than proceed
		// unprotected: a retried plan costs one round trip, an unrestorable
		// checkpoint is found weeks later.
		if err := s.recordCheckpointBlobRefs(checkpointID, plan.RootSHA256, plan.Files); err != nil {
			if errors.Is(err, errCheckpointNotCreating) {
				log.Printf("[checkpoint] rejecting stale plan for %s: checkpoint is no longer creating", shortID(checkpointID))
				http.Error(w, "checkpoint is no longer creating", http.StatusConflict)
				return
			}
			log.Printf("[checkpoint] record blob references for %s: %v", shortID(checkpointID), err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		missing := make([]string, 0)
		for _, f := range plan.Files {
			if f.SHA256 == "" {
				continue
			}
			if s.planBlobMissing(f.SHA256) {
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
	// Claim, then stat -- never a bare stat. A bare stat here answered "already
	// have it" for a blob the sweeper's walker was about to unlink, and this was
	// the one dedup path outside the interlock. See claimBlobPresent.
	if s.claimBlobPresent(sha) {
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
	// An idle checkpoint whose workspace tree is byte-identical to the previous
	// one records that the agent did nothing. On the Faster hub 96% of
	// idle-timer checkpoints were in that state. Recording them as 'ready' work
	// buries the real checkpoints and inflates every count derived from them,
	// so mark the duplicate and stop before writing a manifest.
	if s.checkpointDuplicatesPrevious(checkpointID, clawID, rootSHA) {
		return s.markCheckpointSkipped(checkpointID, rootSHA)
	}
	// Check the guard before writing the manifest, not only after. The guarded
	// UPDATE below is still the authority -- it is what makes the transition
	// atomic against a concurrent one -- but on the common rejection (a late or
	// retried 'complete' for a row that already went terminal) this read means no
	// manifest is written at all, rather than one being written and then having
	// to be cleaned up. The window between this read and the UPDATE is covered by
	// the unlink on rejection further down.
	if err := s.requireCheckpointCreating(s.db.QueryRow(`SELECT status FROM claw_checkpoints WHERE id=?`, checkpointID)); err != nil {
		return err
	}
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
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	manifestSHA := shaBytes(data)
	path := checkpointManifestPath(checkpointID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0o640); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	defer tx.Rollback()
	// The status guard makes this transition apply only to a row still under
	// construction. Without it a late or retried 'complete' could flip a row
	// that had already gone terminal back to 'ready', re-declaring blobs its
	// release had already made collectable.
	res, err := tx.Exec(`UPDATE claw_checkpoints SET status='ready', manifest_sha256=?, manifest_path=?, root_tree_sha256=?, message_tree_sha256=?, workspace_tree_sha256=?, message_count=?, pr_count=?, repo_count=?, pipeline_stage=?, hub_version=?, files_count=?, files_bytes=?, completed_at=? WHERE id=? AND status='creating'`,
		manifestSHA, path, rootSHA, msgSHA, rootSHA, msgCount, len(manifest.PRs), checkpointRepoCount(manifest.PRs),
		manifest.Hub.PipelineStage, manifest.Hub.Version, manifest.FilesCount, manifest.FilesBytes, now(), checkpointID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		// The guard rejected the transition after the manifest was already on
		// disk, so unlink it. A manifest with no row that claims it is debris the
		// hub has no other way to notice.
		_ = os.Remove(path)
		return errCheckpointNotCreating
	}
	// The plan-time edges are KEPT. The root-tree edge and the tree's expansion
	// are the reference to every workspace file this checkpoint holds, and
	// nothing else records them: under schema 2 the per-file digests appear in
	// neither the manifest nor the row. What is added here is what the published
	// checkpoint holds BEYOND the plan -- the message blob, which the claw never
	// planned, plus the manifest digest for the benefit of any future change
	// that stores the manifest as a blob (it is not one today, so the edge is
	// inert). The root edge is re-added defensively; it is already there.
	//
	// The per-file digests are NOT re-inserted here. The plan wrote the
	// expansion, and re-probing ~12k keys under the write lock on every finalize
	// is pure cost. The one probe addTreeBlobRefsTx does run covers the case the
	// plan cannot: a 'complete' naming a root the plan never mentioned, whose
	// tree would otherwise be referenced with no expansion behind it.
	//
	// Same transaction as the UPDATE: a checkpoint that says 'ready' and a
	// complete record of what it holds must land together or not at all.
	if err := addCheckpointBlobRefsTx(tx, checkpointID, []string{rootSHA, msgSHA, manifestSHA}); err != nil {
		return err
	}
	if err := addTreeBlobRefsTx(tx, rootSHA, files); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		// The row is still 'creating' and points at no manifest, so the file just
		// written belongs to nobody.
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (s *Server) completeMetadataOnlyCheckpoint(checkpointID, clawID, reason, detail string) error {
	tenantID, _, _, err := s.clawCheckpointIdentity(clawID)
	if err != nil {
		return err
	}
	// Before writing anything, for the same reason as finalizeCheckpoint.
	if err := s.requireCheckpointCreating(s.db.QueryRow(`SELECT status FROM claw_checkpoints WHERE id=?`, checkpointID)); err != nil {
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
	data, _ := json.Marshal(manifest)
	manifestSHA := shaBytes(data)
	path := checkpointManifestPath(checkpointID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0o640); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE claw_checkpoints SET status='ready', manifest_sha256=?, manifest_path=?, message_tree_sha256=?, message_count=?, pipeline_stage=?, hub_version=?, error=?, completed_at=? WHERE id=? AND status='creating'`,
		manifestSHA, path, msgSHA, msgCount, manifest.Hub.PipelineStage, manifest.Hub.Version, detail, now(), checkpointID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		_ = os.Remove(path)
		return errCheckpointNotCreating
	}
	// A metadata-only capture holds no workspace: the bridge was unreachable, so
	// there was no plan and there are no file blobs. It does hold the message
	// blob it just wrote.
	if err := addCheckpointBlobRefsTx(tx, checkpointID, []string{msgSHA, manifestSHA}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
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
	// Claim, then stat, under the interlock -- the same discipline as the plan
	// handler. The message blob is the one referenced digest the plan never
	// names, so a bare stat here was the one dedup outside the interlock: a
	// sweep that had snapshotted its keep set before this checkpoint's finalize
	// committed could unlink the blob between this stat and that commit.
	if s.claimBlobPresent(sha) {
		return sha, count, cutoff, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", 0, time.Time{}, err
	}
	// Atomic for the same reason manifests are: a half-written blob under a
	// name that asserts its own digest is a lie the restore path believes.
	return sha, count, cutoff, writeFileAtomic(path, buf.Bytes(), 0o640)
}

// touchCheckpointBlob refreshes a reused blob's mtime so the sweep's grace
// window means what it says.
//
// Deduplication is the common case -- blobs are content-addressed and shared
// across claws -- and it returns without writing, so a blob referenced by a
// checkpoint being created right now can carry an mtime from months ago. The
// durable pending-blob claim is the real protection; this keeps the grace
// window from being quietly useless as the second line of defence. A failure
// is not worth failing a checkpoint over, so it is ignored.
func touchCheckpointBlob(path string) {
	at := time.Now()
	_ = os.Chtimes(path, at, at)
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
		Schema:       checkpointManifestSchema,
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
		Provider:   checkpointProvider{Name: row.Provider, ProviderID: row.ProviderID},
		Messages:   checkpointMessages{BlobSHA256: msgSHA, Count: msgCount, CutoffAt: cutoff},
		Workspace:  checkpointWorkspace{TreeSHA256: rootSHA},
		PRs:        prs,
		FilesCount: len(files),
		FilesBytes: filesTotalBytes(files),
	}, nil
}

// filesTotalBytes sums the captured size of a checkpoint's file list. Only the
// total is kept in the manifest; the list itself is addressed by the root tree.
func filesTotalBytes(files []types.CheckpointFile) int64 {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return total
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
			// failure, so do not begin any more uploads once the group is
			// cancelled.
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
	}, 1, func(ctx context.Context, remote string, data []byte) error {
		return sshWriteBytes(ctx, client, remote, data)
	}); err != nil {
		return err
	}
	s.markRestoreApplied(clawID, checkpointID)
	return nil
}

func sshWriteBytes(ctx context.Context, client *gossh.Client, remote string, data []byte) error {
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s", checkpointShellQuote(filepath.Dir(remote)), checkpointShellQuote(remote))
	return sshWriteSessionWithNewSession(ctx, cmd, func() (checkpointSSHSession, error) {
		sess, err := client.NewSession()
		if err != nil {
			return nil, err
		}
		sess.Stdin = bytes.NewReader(data)
		return sess, nil
	}, client.Close)
}

type checkpointSSHSession interface {
	CombinedOutput(string) ([]byte, error)
	Close() error
}

func sshWriteSessionWithNewSession(ctx context.Context, cmd string, newSession func() (checkpointSSHSession, error), closeClient func() error) error {
	type result struct {
		err  error
		sess checkpointSSHSession
	}
	completed := make(chan result, 1)
	go func() {
		sess, err := newSession()
		completed <- result{sess: sess, err: err}
	}()
	select {
	case result := <-completed:
		if result.err != nil {
			return result.err
		}
		return sshWriteSession(ctx, result.sess, cmd, closeClient)
	case <-ctx.Done():
		go func() { _ = closeClient() }()
		return ctx.Err()
	}
}

func sshWriteSession(ctx context.Context, sess checkpointSSHSession, cmd string, closeClient func() error) error {
	type result struct {
		out []byte
		err error
	}
	completed := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		completed <- result{out: out, err: err}
	}()
	select {
	case result := <-completed:
		_ = sess.Close()
		if result.err != nil {
			return fmt.Errorf("%w: %s", result.err, string(result.out))
		}
		return nil
	case <-ctx.Done():
		go func() {
			_ = closeClient()
			_ = sess.Close()
		}()
		return ctx.Err()
	}
}

func checkpointShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// failCheckpoint marks a checkpoint failed and releases every blob it held.
//
// Both happen in one transaction. Releasing without the UPDATE landing was the
// worst of both worlds under ENOSPC or SQLITE_BUSY: the UPDATE fails, the DELETE
// succeeds, and the row is left 'creating' -- still expecting its blobs -- with
// nothing keeping them out of the next sweep.
//
// The status guard makes the transition a no-op on a row that already reached a
// terminal status, so a late 'complete' carrying an error cannot release the
// blobs of a checkpoint that is already ready.
func (s *Server) failCheckpoint(checkpointID, msg string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE claw_checkpoints SET status='failed', error=?, completed_at=? WHERE id=? AND status='creating'`, msg, now(), checkpointID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		log.Printf("[checkpoint] fail for %s ignored: checkpoint is no longer creating", shortID(checkpointID))
		return nil
	}
	if err := deleteCheckpointBlobRefsTx(tx, checkpointID); err != nil {
		return err
	}
	return tx.Commit()
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
