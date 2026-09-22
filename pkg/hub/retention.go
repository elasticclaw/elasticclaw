package hub

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const retentionLive = `('creating','ready','skipped')`
const retentionUnprotected = `NOT EXISTS (SELECT 1 FROM claws p WHERE p.restore_checkpoint_id=c.id OR p.restored_from_checkpoint_id=c.id)
 AND NOT EXISTS (SELECT 1 FROM task_run_attempts a WHERE a.restored_checkpoint_id=c.id)`

var retentionWarnings sync.Map

type retentionSettings struct {
	enabled, dryRun  bool
	interval, maxAge time.Duration
}

type retentionCollection struct {
	trees, blobs, unexpanded int
	bytes                    int64
}

func (s *Server) retentionSettings() retentionSettings {
	cfg := retentionSettings{interval: time.Hour, maxAge: 2160 * time.Hour}
	s.mu.RLock()
	var raw types.RetentionConfig
	if s.hubCfg != nil && s.hubCfg.Retention != nil {
		raw = *s.hubCfg.Retention
	}
	s.mu.RUnlock()
	cfg.enabled, cfg.dryRun = raw.Enabled, raw.DryRun
	parse := func(name, value string, fallback, minimum time.Duration) time.Duration {
		if value == "" {
			return fallback
		}
		d, err := time.ParseDuration(value)
		if err != nil || d < minimum {
			if _, loaded := retentionWarnings.LoadOrStore(name+":"+value, true); !loaded {
				log.Printf("retention: invalid %s %q; using %s", name, value, fallback)
			}
			return fallback
		}
		return d
	}
	cfg.interval = parse("interval", raw.Interval, cfg.interval, 5*time.Minute)
	cfg.maxAge = parse("max_age", raw.MaxAge, cfg.maxAge, 24*time.Hour)
	return cfg
}

func (s *Server) runRetention() {
	cfg := s.retentionSettings()
	if !cfg.enabled {
		log.Printf("retention: disabled")
	}
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("retention: tick panic: %v", r)
				}
			}()
			cfg = s.retentionSettings()
			ticker.Reset(cfg.interval)
			if cfg.enabled {
				s.retainOnce(s.reaperNow(), cfg)
			}
		}()
	}
}

func (s *Server) retainOnce(at time.Time, cfg retentionSettings) {
	unexpanded, err := s.backfillCheckpointTrees(cfg.dryRun)
	if err != nil {
		log.Printf("retention: backfill: %v", err)
		return
	}
	compacted, err := s.compactCheckpoints(at, cfg.dryRun)
	if err != nil {
		log.Printf("retention: compaction: %v", err)
		return
	}
	cutoff := at.Add(-cfg.maxAge)
	expired, err := s.expireCheckpoints(cutoff, cfg.dryRun)
	if err != nil {
		log.Printf("retention: checkpoint expiry: %v", err)
		return
	}
	diagnostics, err := s.expireDiagnostics(cutoff, cfg.dryRun)
	if err != nil {
		log.Printf("retention: diagnostics expiry: %v", err)
		return
	}
	rows, err := s.expireRetentionRows(cutoff, cfg.dryRun)
	if err != nil {
		log.Printf("retention: row expiry: %v", err)
		return
	}
	collected, err := s.collectCheckpointBlobs(at, cfg.dryRun)
	if err != nil {
		log.Printf("retention: collection: %v", err)
		return
	}
	unexpanded = collected.unexpanded
	log.Printf("retention: compacted=%d expired=%d diagnostics=%d rows=%d trees=%d blobs=%d freed=%dMB dry_run=%t unexpanded=%d",
		compacted, expired, diagnostics, rows, collected.trees, collected.blobs, collected.bytes/(1024*1024), cfg.dryRun, unexpanded)
}

func recordCheckpointTree(db *sql.DB, checkpointID, rootSHA string, files []types.CheckpointFile) error {
	if rootSHA == "" {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT OR IGNORE INTO checkpoint_trees(sha256,created_at) VALUES(?,?)`, rootSHA, now())
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted > 0 {
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO checkpoint_tree_files(tree_sha256,file_sha256) VALUES(?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, file := range files {
			if file.SHA256 == "" || file.SHA256 == rootSHA {
				continue
			}
			if _, err := stmt.Exec(rootSHA, file.SHA256); err != nil {
				return err
			}
		}
	}
	if checkpointID != "" {
		if _, err := tx.Exec(`UPDATE claw_checkpoints SET root_tree_sha256=? WHERE id=? AND root_tree_sha256=''`, rootSHA, checkpointID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) backfillCheckpointTrees(dry bool) (int, error) {
	trees, err := retentionStrings(s.db, `SELECT DISTINCT root_tree_sha256 FROM claw_checkpoints
  WHERE root_tree_sha256!='' AND status IN `+retentionLive+`
  AND root_tree_sha256 NOT IN (SELECT sha256 FROM checkpoint_trees) LIMIT 50`)
	if err != nil {
		return 0, err
	}
	for _, tree := range trees {
		files, err := s.filesForTree(tree)
		if err != nil {
			log.Printf("retention: unreadable tree %s: %v", tree, err)
			files = nil
		}
		if dry {
			continue
		}
		if err := recordCheckpointTree(s.db, "", tree, files); err != nil {
			return 0, err
		}
	}
	return s.unexpandedCheckpointTrees()
}

func (s *Server) unexpandedCheckpointTrees() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(DISTINCT root_tree_sha256) FROM claw_checkpoints
  WHERE root_tree_sha256!='' AND status IN ` + retentionLive + `
  AND root_tree_sha256 NOT IN (SELECT sha256 FROM checkpoint_trees)`).Scan(&count)
	return count, err
}

func (s *Server) compactCheckpoints(at time.Time, dry bool) (int, error) {
	rows, err := s.db.Query(`SELECT c.claw_id, l.tenant_id FROM claw_checkpoints c JOIN claws l ON l.id=c.claw_id
  WHERE l.status IN ('error','deleted') AND c.status IN ('ready','skipped','failed','creating')
  GROUP BY c.claw_id HAVING MAX(c.created_at) < ?
  AND NOT EXISTS (SELECT 1 FROM claw_checkpoints recent WHERE recent.claw_id=c.claw_id AND recent.created_at>=?)`, at.Add(-time.Hour), at.Add(-time.Hour))
	if err != nil {
		return 0, err
	}
	type claw struct{ id, tenant string }
	var claws []claw
	for rows.Next() {
		var c claw
		if err := rows.Scan(&c.id, &c.tenant); err != nil {
			rows.Close()
			return 0, err
		}
		claws = append(claws, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range claws {
		keep, _, err := s.selectRetryCheckpoint(c.tenant, c.id, "")
		if err != nil {
			return count, err
		}
		n, err := s.compactCheckpointRows(`c.claw_id=? AND c.id!=? AND c.status IN ('ready','skipped','failed','creating')
   AND EXISTS (SELECT 1 FROM claws l WHERE l.id=c.claw_id AND l.status IN ('error','deleted'))
   AND NOT EXISTS (SELECT 1 FROM claw_checkpoints recent WHERE recent.claw_id=c.claw_id AND recent.created_at>=?)`, dry, c.id, keep, at.Add(-time.Hour))
		count += n
		if err != nil {
			return count, err
		}
	}
	return count, nil
}

func (s *Server) expireCheckpoints(cutoff time.Time, dry bool) (int, error) {
	return s.compactCheckpointRows(`c.created_at<? AND (c.status!='compacted' OR c.manifest_path!='' OR c.root_tree_sha256!='' OR c.workspace_tree_sha256!='')`, dry, cutoff)
}

func (s *Server) compactCheckpointRows(where string, dry bool, args ...any) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	where += " AND " + retentionUnprotected
	paths, err := retentionStrings(tx, `SELECT c.manifest_path FROM claw_checkpoints c WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	if dry {
		return len(paths), nil
	}
	result, err := tx.Exec(`UPDATE claw_checkpoints AS c SET status='compacted',manifest_path='',root_tree_sha256='',workspace_tree_sha256='' WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return int(count), err
		}
	}
	return int(count), nil
}

func (s *Server) expireDiagnostics(cutoff time.Time, dry bool) (int, error) {
	dir := filepath.Join(hubDataDir(), "diagnostics")
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return count, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		count++
		if dry {
			continue
		}
		if err := os.Remove(filepath.Join(dir, file.Name())); err != nil && !os.IsNotExist(err) {
			return count, err
		}
	}
	return count, nil
}

func (s *Server) expireRetentionRows(cutoff time.Time, dry bool) (int, error) {
	count := 0
	for _, table := range []struct {
		name, column string
		cutoff       any
	}{
		{"task_run_events", "event_time", cutoff.UnixMilli()}, {"messages", "created_at", cutoff},
	} {
		predicate := table.column + " < ?"
		var selected int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table.name+` WHERE `+predicate, table.cutoff).Scan(&selected); err != nil {
			return count, err
		}
		count += selected
		if dry {
			continue
		}
		for {
			result, err := s.db.Exec(`DELETE FROM `+table.name+` WHERE rowid IN (SELECT rowid FROM `+table.name+` WHERE `+predicate+` LIMIT 5000)`, table.cutoff)
			if err != nil {
				return count, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return count, err
			}
			if n == 0 {
				break
			}
		}
	}
	return count, nil
}

type retentionQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func retentionStrings(db retentionQuerier, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func retentionUnlink(path string, dry bool, counts *retentionCollection) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if dry {
		counts.blobs++
		counts.bytes += info.Size()
		return nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	counts.blobs++
	counts.bytes += info.Size()
	return nil
}

func (s *Server) collectCheckpointBlobs(at time.Time, dry bool) (retentionCollection, error) {
	var counts retentionCollection
	unexpanded, err := s.unexpandedCheckpointTrees()
	counts.unexpanded = unexpanded
	if err != nil {
		return counts, err
	}
	if unexpanded > 0 {
		log.Printf("retention: collection skipped; unexpanded=%d", unexpanded)
		return counts, nil
	}
	trees, err := retentionStrings(s.db, `SELECT sha256 FROM checkpoint_trees t WHERE NOT EXISTS
  (SELECT 1 FROM claw_checkpoints c WHERE c.root_tree_sha256=t.sha256 AND c.status IN `+retentionLive+`)`)
	if err != nil {
		return counts, err
	}
	for _, tree := range trees {
		if err := s.collectCheckpointTree(tree, dry, &counts); err != nil {
			return counts, err
		}
	}
	messages, err := retentionStrings(s.db, `SELECT DISTINCT message_tree_sha256 FROM claw_checkpoints
  WHERE message_tree_sha256!='' AND status NOT IN `+retentionLive+`
  AND message_tree_sha256 NOT IN (SELECT message_tree_sha256 FROM claw_checkpoints WHERE status IN `+retentionLive+`)`)
	if err != nil {
		return counts, err
	}
	for _, sha := range messages {
		if err := s.collectCheckpointMessage(sha, dry, &counts); err != nil {
			return counts, err
		}
	}
	err = filepath.WalkDir(filepath.Join(checkpointsRoot(), "blobs"), func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.Contains(entry.Name(), ".tmp-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(at.Add(-24 * time.Hour)) {
			return retentionUnlink(path, dry, &counts)
		}
		return nil
	})
	return counts, err
}

func (s *Server) collectCheckpointTree(tree string, dry bool, counts *retentionCollection) error {
	for {
		done, err := s.collectCheckpointTreeBatch(tree, dry, counts)
		if err != nil || done {
			return err
		}
	}
}

func (s *Server) collectCheckpointTreeBatch(tree string, dry bool, counts *retentionCollection) (bool, error) {
	// openDB uses BEGIN IMMEDIATE: plans cannot publish references during unlink.
	tx, err := s.db.Begin()
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	var live bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM claw_checkpoints WHERE root_tree_sha256=? AND status IN `+retentionLive+`)`, tree).Scan(&live); err != nil {
		return true, err
	}
	if live {
		return true, nil
	}
	query := `SELECT file_sha256 FROM checkpoint_tree_files f WHERE tree_sha256=? AND NOT EXISTS
  (SELECT 1 FROM checkpoint_tree_files other WHERE other.file_sha256=f.file_sha256 AND other.tree_sha256!=?)`
	if !dry {
		query += ` LIMIT 500`
	}
	files, err := retentionStrings(tx, query, tree, tree)
	if err != nil {
		return true, err
	}
	if dry {
		for _, sha := range files {
			if err := retentionUnlink(checkpointBlobPath(sha), true, counts); err != nil {
				return true, err
			}
		}
		if err := retentionUnlink(checkpointBlobPath(tree), true, counts); err != nil {
			return true, err
		}
		counts.trees++
		return true, nil
	}
	for _, sha := range files {
		if _, err := tx.Exec(`DELETE FROM checkpoint_tree_files WHERE tree_sha256=? AND file_sha256=?`, tree, sha); err != nil {
			return true, err
		}
		if err := retentionUnlink(checkpointBlobPath(sha), false, counts); err != nil {
			return true, err
		}
	}
	if len(files) > 0 {
		return false, tx.Commit()
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_tree_files WHERE tree_sha256=?`, tree); err != nil {
		return true, err
	}
	if err := retentionUnlink(checkpointBlobPath(tree), false, counts); err != nil {
		return true, err
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_trees WHERE sha256=?`, tree); err != nil {
		return true, err
	}
	if err := tx.Commit(); err != nil {
		return true, err
	}
	counts.trees++
	return true, nil
}

func (s *Server) collectCheckpointMessage(sha string, dry bool, counts *retentionCollection) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var referenced bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM claw_checkpoints WHERE message_tree_sha256=? AND status IN `+retentionLive+`)`, sha).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return nil
	}
	if err := retentionUnlink(checkpointBlobPath(sha), dry, counts); err != nil {
		return fmt.Errorf("unlink message: %w", err)
	}
	if dry {
		return nil
	}
	if _, err := tx.Exec(`UPDATE claw_checkpoints SET message_tree_sha256='' WHERE message_tree_sha256=?`, sha); err != nil {
		return err
	}
	return tx.Commit()
}
