package hub

import (
	"database/sql"
	"log"
	"math"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/harness"
)

var brokenUsageProducers sync.Map

type taskRunUsageBaseline struct {
	epoch       sql.NullString
	in, out     sql.NullInt64
	cost        sql.NullFloat64
	runBaseCost sql.NullFloat64
}

// normalize turns cumulative session totals into the per-run sample consumed by
// the existing pricing and ledger logic, and advances the stored baseline. When
// the producer also sends OpenClaw's flat per-run fields, they stay
// authoritative: a plain difference cannot see a run with the same tokens as
// the previous one, so only cumulative-only producers are billed from the
// difference. A rebase without flat usage bills nothing.
func (b *taskRunUsageBaseline) normalize(snapshot *taskRunUsageSnapshot, found bool, oldIn, oldOut int) (rebase bool) {
	c := snapshot.Cumulative
	flat := snapshot.InputTokens != nil || snapshot.OutputTokens != nil || snapshot.EstimatedCostUSD != nil
	// known: b holds a baseline of this session and epoch, from this row or,
	// for a new row, from the session's previous task run (see inherit).
	known := b.epoch.Valid && b.epoch.String == c.Epoch && b.in.Valid && b.out.Valid && c.InputTokens != nil && c.OutputTokens != nil
	if known && (int64(*c.InputTokens) < b.in.Int64 || int64(*c.OutputTokens) < b.out.Int64) {
		if _, logged := brokenUsageProducers.LoadOrStore(c.Epoch+"/"+c.SessionKey, struct{}{}); !logged {
			log.Printf("[usage] decreasing cumulative tokens for session %s in epoch %s; rebasing", c.SessionKey, c.Epoch)
		}
		known = false
	}
	usable := found && known
	newRun := known && (int64(*c.InputTokens) != b.in.Int64 || int64(*c.OutputTokens) != b.out.Int64)
	switch {
	case flat:
		// Legacy rows, restarted adapters and every OpenClaw sample carry the
		// flat run; record only where it sits in the cumulative totals.
		switch {
		case c.CostUSD != nil && snapshot.EstimatedCostUSD != nil:
			b.runBaseCost = sql.NullFloat64{Float64: *c.CostUSD - *snapshot.EstimatedCostUSD, Valid: true}
		case newRun:
			b.runBaseCost = b.cost
		case !known:
			b.runBaseCost = sql.NullFloat64{}
		}
	case usable:
		// Cumulative-only producer: the run is the growth since the baseline.
		in, out := oldIn, oldOut
		if newRun {
			in, out = *c.InputTokens-int(b.in.Int64), *c.OutputTokens-int(b.out.Int64)
			b.runBaseCost = sql.NullFloat64{Float64: b.cost.Float64, Valid: true}
		}
		snapshot.InputTokens, snapshot.OutputTokens = &in, &out
		if c.CostUSD != nil {
			cost := *c.CostUSD - b.runBaseCost.Float64
			if math.Abs(cost) < 1e-9 {
				cost = 0
			}
			snapshot.EstimatedCostUSD = &cost
		}
	case !found:
		snapshot.InputTokens, snapshot.OutputTokens, snapshot.EstimatedCostUSD = c.InputTokens, c.OutputTokens, c.CostUSD
		b.runBaseCost = sql.NullFloat64{Valid: true}
	default:
		log.Printf("[usage] rebasing cumulative usage for session %s in epoch %s without flat usage", c.SessionKey, c.Epoch)
		b.runBaseCost = sql.NullFloat64{}
		if c.CostUSD != nil {
			b.runBaseCost = sql.NullFloat64{Float64: *c.CostUSD, Valid: true}
		}
		rebase = true
	}
	if !flat {
		if c.Model != "" {
			snapshot.Model = c.Model
		}
		if c.ModelProvider != "" {
			snapshot.ModelProvider = c.ModelProvider
		}
		snapshot.CacheReadTokens, snapshot.CacheWriteTokens = c.CacheReadTokens, c.CacheWriteTokens
	}
	b.epoch = sql.NullString{String: c.Epoch, Valid: true}
	b.in, b.out = sql.NullInt64{}, sql.NullInt64{}
	if c.InputTokens != nil {
		b.in = sql.NullInt64{Int64: int64(*c.InputTokens), Valid: true}
	}
	if c.OutputTokens != nil {
		b.out = sql.NullInt64{Int64: int64(*c.OutputTokens), Valid: true}
	}
	if c.CostUSD != nil {
		b.cost = sql.NullFloat64{Float64: *c.CostUSD, Valid: true}
	}
	return rebase
}

// inherit loads the baseline a new task run row starts from when the session
// already reported usage under another task run, so a later cost correction of
// the run in progress is not charged the session's earlier cost.
func (b *taskRunUsageBaseline) inherit(tx *sql.Tx, tenant, runID string, c *harness.UsageSnapshot) error {
	err := tx.QueryRow(`SELECT cum_epoch,cum_input_tokens,cum_output_tokens,cum_cost_usd,cum_run_base_cost_usd FROM task_run_usage
		WHERE tenant_id=? AND session_key=? AND cum_epoch=? AND run_id<>? ORDER BY updated_at DESC LIMIT 1`, tenant, c.SessionKey, c.Epoch, runID).Scan(
		&b.epoch, &b.in, &b.out, &b.cost, &b.runBaseCost)
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}

func (b taskRunUsageBaseline) store(tx *sql.Tx, tenant, runID, sessionKey string) error {
	_, err := tx.Exec(`UPDATE task_run_usage SET cum_epoch=?,cum_input_tokens=?,cum_output_tokens=?,cum_cost_usd=?,cum_run_base_cost_usd=? WHERE tenant_id=? AND run_id=? AND session_key=?`,
		b.epoch, b.in, b.out, b.cost, b.runBaseCost, tenant, runID, sessionKey)
	return err
}
