package hub

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"time"
)

func (s *Server) successfulClawTerminalStatus(clawID string) string {
	var tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(tags, '[]') FROM claws WHERE id=?`, clawID).Scan(&tagsJSON); err != nil {
		return "deleted"
	}
	var tags []string
	if json.Unmarshal([]byte(tagsJSON), &tags) != nil {
		return "deleted"
	}
	for _, tag := range tags {
		if tag == "routine" {
			return "completed"
		}
	}
	return "deleted"
}

func (s *Server) execStatusLogged(opCtx, query string, args ...any) (sql.Result, error) {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		log.Printf("[status-write] %s failed: %v", opCtx, err)
	}
	return res, err
}

func (s *Server) execTerminalStatus(opCtx, query string, args ...any) (sql.Result, error) {
	res, err := s.db.Exec(query, args...)
	if err == nil {
		return res, nil
	}
	time.Sleep(100 * time.Millisecond)
	res, retryErr := s.db.Exec(query, args...)
	if retryErr != nil {
		log.Printf("[status-write] TERMINAL transition failed twice (%s): %v — leaving for reaper", opCtx, retryErr)
	}
	return res, retryErr
}

// terminalTxOpts tunes side effects applied inside the terminal transaction.
type terminalTxOpts struct {
	// setStopCommentPending marks the claw as owing an issue-tracker comment,
	// so the reaper can re-drive delivery after a crash.
	setStopCommentPending bool
	// clearProviderID drops the VM handle in the same transaction, for callers
	// that already know the VM is gone. Clearing it separately after commit
	// would leave a crash window in which the reaper re-terminates the VM the
	// caller meant to spare.
	clearProviderID bool
}

// finishClawTerminalTx atomically changes a claw's terminal-ish state and its
// running workflow run. A zero-row claw update is an idempotent no-op.
func (s *Server) finishClawTerminalTx(clawID, clawStatus, diagnostic, runStatus, runResult string, opts terminalTxOpts) (bool, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := s.db.Begin()
		if err == nil {
			var res sql.Result
			switch clawStatus {
			case "deleted":
				res, err = tx.Exec(`UPDATE claws SET status=?, bootstrap_status='' WHERE id=? AND status NOT IN ('completed','deleted','error')`, clawStatus, clawID)
			case "idle":
				res, err = tx.Exec(`UPDATE claws SET status=? WHERE id=? AND status NOT IN ('completed','deleted','error')`, clawStatus, clawID)
			default:
				pending := 0
				if opts.setStopCommentPending {
					pending = 1
				}
				res, err = tx.Exec(`UPDATE claws SET status=?, bootstrap_status='', bootstrap_diagnostic=?, stop_comment_pending=? WHERE id=? AND status NOT IN ('completed','deleted','error')`, clawStatus, diagnostic, pending, clawID)
				// Some test/legacy schemas predate the marker migration. The
				// migration runs in production; retain terminal atomicity there too.
				if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such column: stop_comment_pending") {
					res, err = tx.Exec(`UPDATE claws SET status=?, bootstrap_status='', bootstrap_diagnostic=? WHERE id=? AND status NOT IN ('completed','deleted','error')`, clawStatus, diagnostic, clawID)
				}
			}
			if err == nil {
				rows, rowsErr := res.RowsAffected()
				if rowsErr != nil {
					err = rowsErr
				} else if rows == 0 {
					_ = tx.Rollback()
					return false, nil
				}
			}
			if err == nil && opts.clearProviderID {
				_, err = tx.Exec(`UPDATE claws SET provider_id='' WHERE id=?`, clawID)
			}
			if err == nil {
				_, err = tx.Exec(`UPDATE workflow_runs SET status=?, result=?, finished_at=? WHERE claw_id=? AND status='running'`, runStatus, runResult, time.Now().UTC(), clawID)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		if err == nil {
			s.forgetReplicatedBootstrapEnv(clawID)
			return true, nil
		}
		last = err
		if attempt == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	log.Printf("[status-write] TERMINAL transition failed twice (claw %s): %v — leaving for reaper", clawID, last)
	return false, last
}

func isNotFoundTerminateError(err error) bool {
	if err == nil {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") || strings.Contains(s, "404")
}

func (s *Server) terminateVMForClaw(clawID, provider, providerID string) {
	err := s.terminateVMErr(provider, providerID)
	if isNotFoundTerminateError(err) {
		_, _ = s.execStatusLogged("clear provider_id claw "+clawID, `UPDATE claws SET provider_id='' WHERE id=? AND provider_id=?`, clawID, providerID)
		return
	}
	log.Printf("[reaper] VM terminate for claw %s failed: %v", clawID, err)
}
