package hub

import (
	"log"
	"time"
)

const reaperActionLimit = 20

type livenessSettings struct {
	offlineGrace, provisioningMaxAge, claimTTL, interval time.Duration
}

func (s *Server) livenessEnabled() bool {
	if s.hubCfg == nil || s.hubCfg.Liveness == nil || s.hubCfg.Liveness.Enabled == nil {
		return true
	}
	return *s.hubCfg.Liveness.Enabled
}

func (s *Server) livenessSettings() livenessSettings {
	cfg := livenessSettings{10 * time.Minute, 30 * time.Minute, 15 * time.Minute, time.Minute}
	if s.hubCfg == nil || s.hubCfg.Liveness == nil {
		return cfg
	}
	parse := func(value string, fallback time.Duration, name string) time.Duration {
		if value == "" {
			return fallback
		}
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 {
			log.Printf("[reaper] invalid %s %q; using %s", name, value, fallback)
			return fallback
		}
		return d
	}
	l := s.hubCfg.Liveness
	cfg.offlineGrace = parse(l.OfflineGrace, cfg.offlineGrace, "offline_grace")
	cfg.provisioningMaxAge = parse(l.ProvisioningMaxAge, cfg.provisioningMaxAge, "provisioning_max_age")
	cfg.claimTTL = parse(l.ClaimTTL, cfg.claimTTL, "claim_ttl")
	cfg.interval = parse(l.ReaperInterval, cfg.interval, "reaper_interval")
	if cfg.interval <= 0 {
		cfg.interval = time.Minute
	}
	return cfg
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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[reaper] loop panic: %v", r)
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
	rows, err := s.db.Query(`SELECT id, status, created_at FROM claws WHERE status IN ('offline','provisioning','starting')`)
	if err != nil {
		log.Printf("[reaper] claw query: %v", err)
		return
	}
	type claw struct {
		id, status string
		created    time.Time
	}
	var claws []claw
	for rows.Next() {
		var c claw
		if rows.Scan(&c.id, &c.status, &c.created) == nil {
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
		if c.status == "provisioning" || c.status == "starting" {
			if !c.created.IsZero() {
				age = n.Sub(c.created)
			}
		}
		if c.status == "offline" && age > cfg.offlineGrace && take() {
			log.Printf("[reaper] stopping offline claw %s", c.id)
			s.stopAgentWithReason(c.id, "agent offline for "+cfg.offlineGrace.String()+", sandbox presumed dead", false)
		}
		if (c.status == "provisioning" || c.status == "starting") && age > cfg.provisioningMaxAge && take() {
			log.Printf("[reaper] stopping timed out provisioning claw %s", c.id)
			s.stopAgentWithReason(c.id, "provisioning timed out", false)
		}
	}
	s.reaperMu.Lock()
	for key := range s.reaperFirstSeen {
		if len(key) > 5 && key[:5] == "claw:" && !seen[key] {
			delete(s.reaperFirstSeen, key)
		}
	}
	s.reaperMu.Unlock()
	triggers, err := s.db.Query(`SELECT id, created_at FROM factory_triggers WHERE status='claimed' AND claw_id=''`)
	if err == nil {
		var triggerIDs []string
		for triggers.Next() {
			var id string
			var created time.Time
			if triggers.Scan(&id, &created) == nil && n.Sub(s.firstSeen("trigger:"+id, true, n)) > cfg.claimTTL {
				triggerIDs = append(triggerIDs, id)
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
	if actions > 0 {
		s.promotePendingClaws()
	}
}
