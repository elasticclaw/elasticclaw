package hub

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const repeatedNoProgressTurnThreshold = 3

var commitMarkerPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// observeCompletedTurn records the outcome and the external state visible at
// the end of a workflow turn. It pauses automatic continuation only when the
// latest outcomes are substantially the same and that state has not changed.
// This is deliberately progress-based rather than a lifetime or turn limit.
func (s *Server) observeCompletedTurn(clawID, messageID, content string) bool {
	if strings.Contains(content, "[DONE]") || strings.Contains(content, "[TERMINATE]") {
		return false
	}
	s.noProgressMu.Lock()
	defer s.noProgressMu.Unlock()

	var stage string
	var alreadyPaused bool
	if err := s.db.QueryRow(`SELECT pipeline_stage, no_progress_paused != 0 FROM claws WHERE id=?`, clawID).Scan(&stage, &alreadyPaused); err != nil || stage == "" {
		return false
	}
	if alreadyPaused {
		return true
	}

	response := normalizeTurnOutcome(content)
	if response == "" {
		return false
	}
	progress, err := s.turnProgressFingerprint(clawID, content)
	if err != nil {
		log.Printf("[no-progress] fingerprint claw %s: %v", shortID(clawID), err)
		return false
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("[no-progress] begin observation for claw %s: %v", shortID(clawID), err)
		return false
	}
	defer tx.Rollback()

	var previousResponse, previousProgress string
	err = tx.QueryRow(`SELECT response, progress_fingerprint FROM claw_turn_observations WHERE claw_id=? ORDER BY rowid DESC LIMIT 1`, clawID).Scan(&previousResponse, &previousProgress)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[no-progress] load observation for claw %s: %v", shortID(clawID), err)
		return false
	}
	if err == sql.ErrNoRows || previousProgress != progress || !similarTurnOutcomes(previousResponse, response) {
		if _, err := tx.Exec(`DELETE FROM claw_turn_observations WHERE claw_id=?`, clawID); err != nil {
			log.Printf("[no-progress] reset observations for claw %s: %v", shortID(clawID), err)
			return false
		}
	}

	if _, err := tx.Exec(`INSERT OR REPLACE INTO claw_turn_observations(id, claw_id, response, progress_fingerprint, created_at) VALUES(?,?,?,?,?)`, messageID, clawID, response, progress, now()); err != nil {
		log.Printf("[no-progress] store observation for claw %s: %v", shortID(clawID), err)
		return false
	}
	if _, err := tx.Exec(`DELETE FROM claw_turn_observations WHERE claw_id=? AND rowid NOT IN (SELECT rowid FROM claw_turn_observations WHERE claw_id=? ORDER BY rowid DESC LIMIT ?)`, clawID, clawID, repeatedNoProgressTurnThreshold); err != nil {
		log.Printf("[no-progress] prune observations for claw %s: %v", shortID(clawID), err)
		return false
	}
	var repeats int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM claw_turn_observations WHERE claw_id=?`, clawID).Scan(&repeats); err != nil {
		return false
	}
	paused := repeats >= repeatedNoProgressTurnThreshold
	if paused {
		if _, err := tx.Exec(`UPDATE claws SET no_progress_paused=1 WHERE id=?`, clawID); err != nil {
			return false
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[no-progress] commit observation for claw %s: %v", shortID(clawID), err)
		return false
	}
	if !paused {
		return false
	}

	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc != nil {
		cc.mu.Lock()
		cc.noProgressPaused = true
		cc.mu.Unlock()
	}
	log.Printf("[no-progress] paused automatic continuation for claw %s after %d repeated outcomes", shortID(clawID), repeats)
	s.publishHubNotice(clawID, "[hub] Automatic continuation paused: the last three agent turns reported nearly the same outcome while workflow, CI, review, and pull-request state did not change. Send a message to resume after reviewing the activity.")
	return true
}

func (s *Server) resumeNoProgressAfterUserInput(clawID string) {
	s.noProgressMu.Lock()
	defer s.noProgressMu.Unlock()

	res, err := s.db.Exec(`UPDATE claws SET no_progress_paused=0 WHERE id=? AND no_progress_paused!=0`, clawID)
	if err != nil {
		log.Printf("[no-progress] resume claw %s: %v", shortID(clawID), err)
		return
	}
	_, _ = s.db.Exec(`DELETE FROM claw_turn_observations WHERE claw_id=?`, clawID)
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc != nil {
		cc.mu.Lock()
		cc.noProgressPaused = false
		cc.mu.Unlock()
	}
	if changed, _ := res.RowsAffected(); changed > 0 {
		log.Printf("[no-progress] resumed claw %s after new user or external input", shortID(clawID))
	}
}

// publishHubNotice persists a dashboard-visible hub message without turning it
// into another model prompt. It is used for bookkeeping and intervention
// notices whose contents should not consume an agent turn.
func (s *Server) publishHubNotice(clawID, content string) {
	s.publishHubNoticeWithFormat(clawID, content, "pre")
}

func (s *Server) publishHubNoticeWithFormat(clawID, content, format string) {
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		return
	}
	createdAt := now()
	msg := types.HubMessage{
		ID: uuid.New().String(), ClawID: clawID, TenantID: tenantID,
		Role: "hub", Content: content, Format: format, CreatedAt: createdAt,
	}
	if _, err := s.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`, msg.ID, clawID, tenantID, msg.Role, msg.Content, msg.Format, createdAt, createdAt); err != nil {
		log.Printf("[hub] persist notice for claw %s: %v", shortID(clawID), err)
		return
	}
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: msg})
}

func normalizeTurnOutcome(content string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(strings.TrimSpace(content)) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func similarTurnOutcomes(a, b string) bool {
	if a == b {
		return true
	}
	aWords := strings.Fields(a)
	bWords := strings.Fields(b)
	if len(aWords) < 12 || len(bWords) < 12 {
		return false
	}
	aSet := make(map[string]struct{}, len(aWords))
	bSet := make(map[string]struct{}, len(bWords))
	for _, word := range aWords {
		aSet[word] = struct{}{}
	}
	for _, word := range bWords {
		bSet[word] = struct{}{}
	}
	intersection := 0
	for word := range aSet {
		if _, ok := bSet[word]; ok {
			intersection++
		}
	}
	union := len(aSet) + len(bSet) - intersection
	return union > 0 && float64(intersection)/float64(union) >= 0.8
}

func (s *Server) turnProgressFingerprint(clawID, response string) (string, error) {
	var parts []string
	var stage, taskRunID string
	if err := s.db.QueryRow(`SELECT pipeline_stage, task_run_id FROM claws WHERE id=?`, clawID).Scan(&stage, &taskRunID); err != nil {
		return "", fmt.Errorf("load claw state: %w", err)
	}
	parts = append(parts, "stage="+stage)

	rows, err := s.db.Query(`SELECT repo, pr_number, last_ci_sha, last_comment_id, last_comment_at, last_review_comment_id, last_review_id, pr_conditions_fired FROM claw_prs WHERE claw_id=? ORDER BY repo, pr_number`, clawID)
	if err != nil {
		return "", fmt.Errorf("query tracked pull requests: %w", err)
	}
	for rows.Next() {
		var repo, ciSHA, commentAt string
		var number, commentID, reviewCommentID, reviewID, conditions int
		if err := rows.Scan(&repo, &number, &ciSHA, &commentID, &commentAt, &reviewCommentID, &reviewID, &conditions); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan tracked pull request: %w", err)
		}
		parts = append(parts, fmt.Sprintf("pr=%s#%d:%s:%d:%s:%d:%d:%d", repo, number, ciSHA, commentID, commentAt, reviewCommentID, reviewID, conditions))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("iterate tracked pull requests: %w", err)
	}
	rows.Close()

	if taskRunID != "" {
		rows, err = s.db.Query(`SELECT repo, pr_number, head_sha, last_agent_head_sha, state, merged FROM task_run_prs WHERE run_id=? ORDER BY repo, pr_number`, taskRunID)
		if err != nil {
			return "", fmt.Errorf("query task pull requests: %w", err)
		}
		for rows.Next() {
			var repo, headSHA, agentSHA, state string
			var number, merged int
			if err := rows.Scan(&repo, &number, &headSHA, &agentSHA, &state, &merged); err != nil {
				rows.Close()
				return "", fmt.Errorf("scan task pull request: %w", err)
			}
			parts = append(parts, fmt.Sprintf("task-pr=%s#%d:%s:%s:%s:%d", repo, number, headSHA, agentSHA, state, merged))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", fmt.Errorf("iterate task pull requests: %w", err)
		}
		rows.Close()
	}
	rows, err = s.db.Query(`SELECT output_name, exit_code, stdout, stderr, parsed_json FROM pipeline_outputs WHERE claw_id=? ORDER BY output_name`, clawID)
	if err != nil {
		return "", fmt.Errorf("query pipeline outputs: %w", err)
	}
	for rows.Next() {
		var name, stdout, stderr, parsed string
		var exitCode int
		if err := rows.Scan(&name, &exitCode, &stdout, &stderr, &parsed); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan pipeline output: %w", err)
		}
		parts = append(parts, fmt.Sprintf("output=%s:%d:%s:%s:%s", name, exitCode, stdout, stderr, parsed))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("iterate pipeline outputs: %w", err)
	}
	rows.Close()

	parts = append(parts, responseProgressMarkers(response)...)
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:]), nil
}

func responseProgressMarkers(content string) []string {
	markers := make([]string, 0)
	for _, pr := range extractPRs(content) {
		markers = append(markers, "mentioned-pr="+pr.url)
	}
	for _, sha := range commitMarkerPattern.FindAllString(strings.ToLower(content), -1) {
		if strings.ContainsAny(sha, "abcdef") && strings.ContainsAny(sha, "0123456789") {
			markers = append(markers, "mentioned-sha="+sha)
		}
	}
	sort.Strings(markers)
	return markers
}
