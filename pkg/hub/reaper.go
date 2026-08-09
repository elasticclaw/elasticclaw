package hub

import (
	"log"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const reaperActionLimit = 20
const redriveGrace = 2 * time.Minute

// terminalStageRecoveryGrace must exceed the legitimate in-flight window of a
// terminal pipeline transition (streaming wait ~30s + checkpoint wait 90s), so
// the reaper never completes a stage the main path is still finishing.
const terminalStageRecoveryGrace = 5 * time.Minute

type livenessSettings struct {
	offlineGrace, provisioningMaxAge, claimTTL, interval time.Duration
	gatewayUnhealthyMax                                  int
	busyTurnMax, silentDeathMax                          time.Duration
	prConditionsMaxWait                                  time.Duration
}

func (s *Server) livenessEnabled() bool {
	s.mu.RLock()
	liveness := livenessConfig(s.hubCfg)
	s.mu.RUnlock()
	if liveness == nil || liveness.Enabled == nil {
		return true
	}
	return *liveness.Enabled
}

func (s *Server) livenessSettings() livenessSettings {
	cfg := livenessSettings{
		offlineGrace:        10 * time.Minute,
		provisioningMaxAge:  30 * time.Minute,
		claimTTL:            15 * time.Minute,
		interval:            time.Minute,
		gatewayUnhealthyMax: defaultGatewayUnhealthyMax,
		busyTurnMax:         defaultBusyTurnMax,
		silentDeathMax:      defaultSilentDeathMax,
		prConditionsMaxWait: 2 * time.Hour,
	}
	s.mu.RLock()
	l := livenessConfig(s.hubCfg)
	s.mu.RUnlock()
	if l == nil {
		return cfg
	}
	parse := func(value string, fallback time.Duration, name string) time.Duration {
		if value == "" {
			return fallback
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			log.Printf("[reaper] invalid %s %q; using %s", name, value, fallback)
			return fallback
		}
		return d
	}
	cfg.offlineGrace = parse(l.OfflineGrace, cfg.offlineGrace, "offline_grace")
	cfg.provisioningMaxAge = parse(l.ProvisioningMaxAge, cfg.provisioningMaxAge, "provisioning_max_age")
	cfg.claimTTL = parse(l.ClaimTTL, cfg.claimTTL, "claim_ttl")
	cfg.interval = parse(l.ReaperInterval, cfg.interval, "reaper_interval")
	cfg.busyTurnMax = parse(l.BusyTurnMax, cfg.busyTurnMax, "busy_turn_max")
	cfg.silentDeathMax = parse(l.SilentDeathMax, cfg.silentDeathMax, "silent_death_max")
	cfg.prConditionsMaxWait = parse(l.PRConditionsMaxWait, cfg.prConditionsMaxWait, "pr_conditions_max_wait")
	if l.GatewayUnhealthyChecks != nil {
		if *l.GatewayUnhealthyChecks <= 0 {
			log.Printf("[reaper] invalid gateway_unhealthy_checks %d; using %d", *l.GatewayUnhealthyChecks, cfg.gatewayUnhealthyMax)
		} else {
			cfg.gatewayUnhealthyMax = *l.GatewayUnhealthyChecks
		}
	}
	return cfg
}

func livenessConfig(cfg *types.HubConfig) *types.LivenessConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Liveness
}

func (s *Server) reaperNow() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc().UTC()
	}
	return now()
}

// reconcileOnBoot handles work whose owner was the previous hub process.
func (s *Server) reconcileOnBoot() {
	rows, err := s.db.Query(`SELECT id, COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE status IN ('provisioning','starting')`)
	if err != nil {
		log.Printf("[reaper] boot provisioning query: %v", err)
		return
	}
	defer rows.Close()
	type claw struct{ id, provider, providerID string }
	var claws []claw
	for rows.Next() {
		var c claw
		if rows.Scan(&c.id, &c.provider, &c.providerID) == nil {
			claws = append(claws, c)
		}
	}
	for _, c := range claws {
		if c.provider == "replicated" && c.providerID != "" {
			continue
		}
		log.Printf("[reaper] boot stopping claw %s stranded during provisioning", c.id)
		s.stopAgentWithReason(c.id, "hub restarted during provisioning", false)
	}
	n := s.reaperNow()
	if res, err := s.db.Exec(`UPDATE workflow_runs SET status='failed', result='orphaned by hub restart', finished_at=? WHERE status='running' AND (claw_id='' OR NOT EXISTS (SELECT 1 FROM claws c WHERE c.id=workflow_runs.claw_id) OR EXISTS (SELECT 1 FROM claws c WHERE c.id=workflow_runs.claw_id AND c.status IN ('error','deleted')))`, n); err != nil {
		log.Printf("[reaper] boot workflow repair: %v", err)
	} else if count, _ := res.RowsAffected(); count > 0 {
		log.Printf("[reaper] boot failed %d orphaned workflow runs", count)
	}
	if res, err := s.db.Exec(`UPDATE factory_triggers SET status='failed', updated_at=? WHERE status='claimed' AND claw_id=''`, n); err != nil {
		log.Printf("[reaper] boot trigger repair: %v", err)
	} else if count, _ := res.RowsAffected(); count > 0 {
		log.Printf("[reaper] boot failed %d unassigned trigger claims", count)
	}
	if res, err := s.db.Exec(`UPDATE claw_checkpoints SET status='failed', error='hub restarted while checkpoint was creating', completed_at=? WHERE status='creating'`, n); err != nil {
		log.Printf("[reaper] boot checkpoint repair: %v", err)
	} else if count, _ := res.RowsAffected(); count > 0 {
		log.Printf("[reaper] boot failed %d creating checkpoints", count)
	}
	s.promotePendingClaws()
}

func (s *Server) runReaper() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[reaper] loop panic, restarting: %v", r)
				}
			}()
			ticker := time.NewTicker(s.livenessSettings().interval)
			defer ticker.Stop()
			for range ticker.C {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[reaper] tick panic: %v", r)
						}
					}()
					s.reapOnce()
				}()
			}
		}()
	}
}

func (s *Server) firstSeen(key string, present bool, at time.Time) time.Time {
	s.reaperMu.Lock()
	defer s.reaperMu.Unlock()
	if !present {
		delete(s.reaperFirstSeen, key)
		return time.Time{}
	}
	if seen := s.reaperFirstSeen[key]; !seen.IsZero() {
		return seen
	}
	s.reaperFirstSeen[key] = at
	return at
}

func (s *Server) reapOnce() {
	cfg, n, actions := s.livenessSettings(), s.reaperNow(), 0
	take := func() bool {
		if actions >= reaperActionLimit {
			return false
		}
		actions++
		return true
	}
	previewRows, err := s.db.Query(
		`SELECT id, tenant_id, COALESCE(provider,''), COALESCE(provider_id,'')
		   FROM claws
		  WHERE status='preview' AND preview_expires_at > 0 AND preview_expires_at <= ?`,
		n.UnixMilli(),
	)
	if err == nil {
		type expiredPreview struct{ id, tenantID, provider, providerID string }
		var previews []expiredPreview
		for previewRows.Next() {
			var preview expiredPreview
			if previewRows.Scan(&preview.id, &preview.tenantID, &preview.provider, &preview.providerID) == nil {
				previews = append(previews, preview)
			}
		}
		previewRows.Close()
		for _, preview := range previews {
			if !take() {
				break
			}
			applied, finishErr := s.finishClawTerminalTx(
				preview.id,
				"deleted",
				"",
				"completed",
				"QA preview TTL expired",
				terminalTxOpts{},
			)
			if finishErr != nil {
				log.Printf("[reaper] expire preview %s: %v", preview.id, finishErr)
				continue
			}
			if !applied {
				continue
			}
			_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, preview.id)
			if s.cronScheduler != nil {
				s.cronScheduler.releaseClawWorkflowSlot(preview.id)
			}
			s.broadcastToUsers(preview.tenantID, types.WSMessage{
				Type:    "claw_status",
				Payload: map[string]string{"claw_id": preview.id, "status": "deleted"},
			})
			if preview.providerID != "" {
				go s.terminateVMForClaw(preview.id, preview.provider, preview.providerID)
			}
			log.Printf("[reaper] expired QA preview for claw %s", preview.id)
		}
	} else {
		log.Printf("[reaper] preview query: %v", err)
	}
	rows, err := s.db.Query(`SELECT id, status FROM claws WHERE status IN ('offline','provisioning','starting')`)
	if err != nil {
		log.Printf("[reaper] claw query: %v", err)
		return
	}
	type claw struct {
		id, status string
	}
	var claws []claw
	for rows.Next() {
		var c claw
		if rows.Scan(&c.id, &c.status) == nil {
			claws = append(claws, c)
		}
	}
	rows.Close()
	seen := make(map[string]bool, len(claws))
	for _, c := range claws {
		key := "claw:" + c.id + ":" + c.status
		seen[key] = true
		first := s.firstSeen(key, true, n)
		age := n.Sub(first)
		if c.status == "offline" && age > cfg.offlineGrace && take() {
			log.Printf("[reaper] stopping offline claw %s", c.id)
			s.stopAgentWithReason(c.id, "agent offline for "+cfg.offlineGrace.String()+", sandbox presumed dead", false)
		}
		if (c.status == "provisioning" || c.status == "starting") && age > cfg.provisioningMaxAge && take() {
			log.Printf("[reaper] stopping timed out provisioning claw %s", c.id)
			s.stopAgentWithReason(c.id, "provisioning timed out", false)
		}
	}
	type vm struct{ id, provider, providerID string }
	vmRows, err := s.db.Query(`SELECT id, COALESCE(provider,''), provider_id FROM claws WHERE status IN ('error','deleted') AND provider_id != ''`)
	if err == nil {
		var vms []vm
		for vmRows.Next() {
			var v vm
			if vmRows.Scan(&v.id, &v.provider, &v.providerID) == nil {
				vms = append(vms, v)
			}
		}
		vmRows.Close()
		for _, v := range vms {
			key := "vm:" + v.id
			seen[key] = true
			if n.Sub(s.firstSeen(key, true, n)) > redriveGrace && take() {
				log.Printf("[reaper] re-driving VM terminate for claw %s", v.id)
				go s.terminateVMForClaw(v.id, v.provider, v.providerID)
			}
		}
	}
	commentRows, err := s.db.Query(`SELECT id FROM claws WHERE status='error' AND stop_comment_pending=1`)
	if err == nil {
		var commentIDs []string
		for commentRows.Next() {
			var id string
			if commentRows.Scan(&id) == nil {
				commentIDs = append(commentIDs, id)
			}
		}
		commentRows.Close()
		for _, id := range commentIDs {
			key := "comment:" + id
			seen[key] = true
			if n.Sub(s.firstSeen(key, true, n)) <= redriveGrace || !take() {
				continue
			}
			var reason string
			_ = s.db.QueryRow(`SELECT COALESCE(bootstrap_diagnostic,'') FROM claws WHERE id=?`, id).Scan(&reason)
			if ctx, ok := s.findPipelineContextForClaw(id); ok && ctx.Workflow != nil && ctx.IssueID != "" {
				s.dispatchWorkflowStopComment(id, ctx, reason)
			} else if f, issueID := s.findFactoryForClaw(id); f != nil && issueID != "" {
				s.dispatchFactoryStopComment(id, f, issueID, reason)
			} else {
				log.Printf("[reaper] clearing unresolved stop comment for claw %s", id)
				_, _ = s.execStatusLogged("clear stop comment claw "+id, `UPDATE claws SET stop_comment_pending=0 WHERE id=?`, id)
			}
		}
	}
	// A terminal pipeline stage is claimed before its terminal transaction is
	// committed. If that transaction exhausts its short retry budget, recover it
	// here rather than leaving the claimed stage and workflow run running forever.
	terminalRows, err := s.db.Query(`SELECT c.id, c.pipeline_stage FROM claws c WHERE c.status NOT IN ('error','deleted') AND c.pipeline_stage != '' AND EXISTS (SELECT 1 FROM workflow_runs wr WHERE wr.claw_id=c.id AND wr.status='running')`)
	if err == nil {
		type terminalCandidate struct{ id, stageID string }
		var candidates []terminalCandidate
		for terminalRows.Next() {
			var c terminalCandidate
			if terminalRows.Scan(&c.id, &c.stageID) == nil {
				candidates = append(candidates, c)
			}
		}
		terminalRows.Close()
		for _, c := range candidates {
			ctx, ok := s.findPipelineContextForClaw(c.id)
			stage := parsePipelineForContext(ctx)
			if !ok || stage == nil || stage.StageByID(c.stageID) == nil || !stage.StageByID(c.stageID).Terminal {
				continue
			}
			// The main termination path legitimately holds this state while it
			// waits for streaming and a best-effort checkpoint; only recover
			// stages that have been stuck well past that window.
			key := "terminal:" + c.id
			seen[key] = true
			if n.Sub(s.firstSeen(key, true, n)) <= terminalStageRecoveryGrace || !take() {
				continue
			}
			status, result := pipelineTerminalWorkflowRunResult(*stage.StageByID(c.stageID), true)
			if applied, e := s.finishClawTerminalTx(c.id, "deleted", "", status, result, terminalTxOpts{}); e == nil && applied {
				log.Printf("[reaper] completed stranded terminal pipeline stage for claw %s", c.id)
				if s.cronScheduler != nil {
					s.cronScheduler.releaseClawWorkflowSlot(c.id)
				}
				var provider, providerID string
				_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=?`, c.id).Scan(&provider, &providerID)
				if providerID != "" {
					go s.terminateVMForClaw(c.id, provider, providerID)
				}
			}
		}
	}
	triggers, err := s.db.Query(`SELECT id, created_at FROM factory_triggers WHERE status='claimed' AND claw_id=''`)
	if err == nil {
		var triggerIDs []string
		for triggers.Next() {
			var id string
			var created time.Time
			if triggers.Scan(&id, &created) == nil {
				seen["trigger:"+id] = true
				if n.Sub(s.firstSeen("trigger:"+id, true, n)) > cfg.claimTTL {
					triggerIDs = append(triggerIDs, id)
				}
			}
		}
		triggers.Close()
		for _, id := range triggerIDs {
			if !take() {
				break
			}
			if _, e := s.db.Exec(`UPDATE factory_triggers SET status='failed', updated_at=? WHERE id=? AND status='claimed' AND claw_id=''`, n, id); e == nil {
				log.Printf("[reaper] failed expired trigger claim %s", id)
			}
		}
	}
	timeouts, err := s.db.Query(`SELECT DISTINCT c.id FROM task_runs tr JOIN claws c ON c.id=tr.claw_id WHERE tr.timeout_at > 0 AND tr.timeout_at < ? AND c.status IN ('provisioning','starting','connected','idle','offline')`, n.UnixMilli())
	if err == nil {
		var ids []string
		for timeouts.Next() {
			var id string
			if timeouts.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		timeouts.Close()
		for _, id := range ids {
			if !take() {
				break
			}
			log.Printf("[reaper] stopping claw %s for task timeout", id)
			s.stopAgentWithReason(id, "task run exceeded its timeout", false)
		}
	}
	// Evict firstSeen entries for claws/triggers no longer in a reap-eligible
	// state, so the map cannot grow monotonically.
	s.reaperMu.Lock()
	for key := range s.reaperFirstSeen {
		if !seen[key] {
			delete(s.reaperFirstSeen, key)
		}
	}
	s.reaperMu.Unlock()
	if actions > 0 {
		s.promotePendingClaws()
	}
}
