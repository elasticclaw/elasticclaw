package aichat

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
)

type SweepStats struct {
	ArchivedThreads    int
	ClearedToolResults int
	DeletedVersions    int
	RemovedFiles       int
	SkippedFiles       int
	Errors             int
	archived           []Thread
}

// StartRetention follows the hub context and returns a shutdown join. Configuration
// is rediscovered each day so adding or removing ai_chat.yaml needs no restart.
func StartRetention(parent context.Context, deps Deps) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-timer.C:
				if _, err := deps.sweep(ctx, now); err != nil && ctx.Err() == nil {
					log.Print("[ai-chat] retention sweep failed")
				}
				timer.Reset(24 * time.Hour)
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (d Deps) sweep(ctx context.Context, now time.Time) (SweepStats, error) {
	if d.FeatureStage == nil {
		return SweepStats{}, nil
	}
	stage := d.FeatureStage(ctx, "ai-chat")
	if stage != "on" && stage != "beta" {
		return SweepStats{}, nil
	}
	names, err := d.Workspaces()
	if err != nil {
		return SweepStats{}, err
	}
	configured := false
	retention := map[string]config.Retention{}
	for _, name := range names {
		dir := d.ManagedDir(name)
		if info, err := os.Stat(filepath.Join(dir, "ai_chat.yaml")); err == nil && info.Mode().IsRegular() {
			configured = true
		}
		cfg, err := config.Load(dir)
		switch {
		case err == nil:
			retention[name] = cfg.Retention
		case os.IsNotExist(err):
			retention[name] = config.Retention{}
		default:
			// Do not log configuration contents, paths or parser errors.
			log.Print("[ai-chat] retention skipped a workspace: configuration unavailable")
		}
	}
	if !configured {
		return SweepStats{}, nil
	}
	stats, err := (&Store{DB: d.DB}).Sweep(ctx, now, func(workspace string) (config.Retention, bool) {
		r, ok := retention[workspace]
		return r, ok
	}, d.ArtifactsDir)
	for _, t := range stats.archived {
		if ctx.Err() != nil {
			break
		}
		if d.Publish != nil {
			d.Publish(t)
		}
	}
	log.Printf("[ai-chat] retention archived=%d tool_results=%d versions=%d files=%d skipped=%d errors=%d", stats.ArchivedThreads, stats.ClearedToolResults, stats.DeletedVersions, stats.RemovedFiles, stats.SkippedFiles, stats.Errors)
	return stats, err
}

func retentionDefaults(r config.Retention) config.Retention {
	if r.ChatsDays <= 0 {
		r.ChatsDays = 90
	}
	if r.ToolResultsDays <= 0 {
		r.ToolResultsDays = 30
	}
	if r.ArtifactVersionsDays <= 0 {
		r.ArtifactVersionsDays = 30
	}
	return r
}

// Sweep uses separate short transactions for archival, tool results and each
// version. Ticket results are durable action state, not expiring source data.
// retentionFor returns false when a workspace's retention cannot be loaded.
func (s *Store) Sweep(ctx context.Context, now time.Time, retentionFor func(string) (config.Retention, bool), artifactsDir string) (SweepStats, error) {
	var stats SweepStats
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT tenant_id,owner_login,workspace FROM ai_chat_threads`)
	if err != nil {
		return stats, err
	}
	type scope struct{ tenant, owner, workspace string }
	var scopes []scope
	for rows.Next() {
		var v scope
		if err = rows.Scan(&v.tenant, &v.owner, &v.workspace); err != nil {
			rows.Close()
			return stats, err
		}
		scopes = append(scopes, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return stats, err
	}
	for _, v := range scopes {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		retention, ok := retentionFor(v.workspace)
		if !ok {
			continue
		}
		retention = retentionDefaults(retention)
		cutoff := func(days int) int64 { return now.AddDate(0, 0, -days).UnixMilli() }
		archived, err := s.archiveExpired(ctx, v.tenant, v.owner, v.workspace, cutoff(retention.ChatsDays), now.UnixMilli())
		if err != nil {
			return stats, err
		}
		stats.ArchivedThreads += len(archived)
		stats.archived = append(stats.archived, archived...)
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return stats, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE ai_chat_tool_runs SET result_json=NULL WHERE result_json IS NOT NULL AND created_at<? AND NOT (tool='ticket' AND provider='AI Chat') AND message_id IN (SELECT m.id FROM ai_chat_messages m JOIN ai_chat_threads t ON t.id=m.thread_id WHERE t.tenant_id=? AND t.owner_login=? AND t.workspace=?)`, cutoff(retention.ToolResultsDays), v.tenant, v.owner, v.workspace)
		if err != nil {
			tx.Rollback()
			return stats, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			tx.Rollback()
			return stats, err
		}
		if err = tx.Commit(); err != nil {
			return stats, err
		}
		stats.ClearedToolResults += int(n)
		rows, err := s.DB.QueryContext(ctx, `SELECT a.id,v.version FROM ai_chat_artifact_versions v JOIN ai_chat_artifacts a ON a.id=v.artifact_id WHERE a.tenant_id=? AND a.owner_login=? AND a.workspace=? AND (a.deleted_at IS NOT NULL OR (v.version<a.current_version AND v.created_at<?))`, v.tenant, v.owner, v.workspace, cutoff(retention.ArtifactVersionsDays))
		if err != nil {
			return stats, err
		}
		type version struct {
			id     string
			number int
		}
		var versions []version
		for rows.Next() {
			var item version
			if err = rows.Scan(&item.id, &item.number); err != nil {
				rows.Close()
				return stats, err
			}
			versions = append(versions, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return stats, err
		}
		for _, item := range versions {
			if err = s.sweepVersion(ctx, artifactsDir, v.tenant, v.owner, item.id, item.number, cutoff(retention.ArtifactVersionsDays), &stats); err != nil {
				if ctx.Err() != nil {
					return stats, ctx.Err()
				}
				stats.Errors++
				log.Print("[ai-chat] retention failed to sweep an artifact version")
			}
		}
	}
	return stats, nil
}

func (s *Store) archiveExpired(ctx context.Context, tenant, owner, workspace string, cutoff, now int64) ([]Thread, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Acquire refreshes updated_at before preparation. Also exclude persisted
	// recent streaming messages; crash remnants must eventually expire too.
	rows, err := tx.QueryContext(ctx, `UPDATE ai_chat_threads SET archived_at=? WHERE tenant_id=? AND owner_login=? AND workspace=? AND archived_at IS NULL AND updated_at<? AND NOT EXISTS(SELECT 1 FROM ai_chat_messages m WHERE m.thread_id=ai_chat_threads.id AND m.status='streaming' AND m.created_at>?) RETURNING `+threadColumns, now, tenant, owner, workspace, cutoff, now-ArtifactTimeout.Milliseconds())
	if err != nil {
		return nil, err
	}
	var archived []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		archived = append(archived, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return archived, tx.Commit()
}

func (s *Store) sweepVersion(ctx context.Context, dir, tenant, owner, id string, version int, cutoff int64, stats *SweepStats) error {
	artifactWriteMu.Lock()
	defer artifactWriteMu.Unlock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var path string
	var deleted bool
	err = tx.QueryRowContext(ctx, `SELECT v.path,a.deleted_at IS NOT NULL FROM ai_chat_artifact_versions v JOIN ai_chat_artifacts a ON a.id=v.artifact_id WHERE a.id=? AND a.tenant_id=? AND a.owner_login=? AND v.version=? AND (a.deleted_at IS NOT NULL OR (v.version<a.current_version AND v.created_at<?))`, id, tenant, owner, version, cutoff).Scan(&path, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	removed, unsafe, err := removeArtifactFile(dir, path)
	if unsafe {
		stats.SkippedFiles++
		// Do not log untrusted paths, titles or configuration values.
		log.Print("[ai-chat] retention skipped a path outside the artifacts directory")
		return nil
	}
	if err != nil {
		return err
	}
	if removed {
		stats.RemovedFiles++
	}
	if !deleted {
		if _, err = tx.ExecContext(ctx, `DELETE FROM ai_chat_artifact_versions WHERE artifact_id=? AND version=?`, id, version); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if !deleted {
		stats.DeletedVersions++
	}
	return nil
}

func removeArtifactFile(dir, path string) (removed, unsafe bool, err error) {
	if dir == "" || !filepath.IsAbs(path) {
		return false, true, nil
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return false, true, nil
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, true, nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	relative, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false, true, nil
	}
	// Root.Remove also confines the operation if a symlink changes after validation.
	confined, err := os.OpenRoot(root)
	if err != nil {
		return false, false, err
	}
	defer confined.Close()
	err = confined.Remove(rel)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	return err == nil, false, err
}
