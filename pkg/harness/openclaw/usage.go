package openclaw

import "github.com/elasticclaw/elasticclaw/pkg/harness"

// UsageAccumulator converts OpenClaw's current-run totals to session totals.
// Its caller serializes Observe calls with the sample's own lock.
type UsageAccumulator struct {
	epoch    string
	sessions map[string]*sessionUsage
}

// sessionUsage keeps one session's totals so that returning to an earlier
// session key resumes its totals instead of restarting them below the
// baseline the hub already stored for that key.
type sessionUsage struct {
	baseIn, baseOut int
	baseCost        float64
	lastIn, lastOut int
	lastCost        float64
	hasIn, hasOut   bool
}

func NewUsageAccumulator(epoch string) *UsageAccumulator {
	return &UsageAccumulator{epoch: epoch, sessions: make(map[string]*sessionUsage)}
}

func (a *UsageAccumulator) Observe(sessionKey string, in, out *int, cost *float64, model, provider string, cacheRead, cacheWrite *int) harness.UsageSnapshot {
	u := a.sessions[sessionKey]
	if u == nil {
		u = &sessionUsage{}
		a.sessions[sessionKey] = u
	}
	// Missing token fields keep the last seen values, like the hub's row.
	input, output := u.lastIn, u.lastOut
	if in != nil {
		input, u.hasIn = *in, true
	}
	if out != nil {
		output, u.hasOut = *out, true
	}
	// OpenClaw overwrites a session entry after each reply with that run's
	// totals, so changed tokens mean the previous run is complete.
	if input != u.lastIn || output != u.lastOut {
		u.baseIn += u.lastIn
		u.baseOut += u.lastOut
		u.baseCost += u.lastCost
		u.lastIn, u.lastOut, u.lastCost = input, output, 0
	}
	if cost != nil {
		u.lastCost = *cost
	}
	snapshot := harness.UsageSnapshot{
		Epoch: a.epoch, SessionKey: sessionKey, Model: model, ModelProvider: provider,
		CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
	}
	if u.hasIn || u.hasOut {
		inputTotal, outputTotal := u.baseIn+u.lastIn, u.baseOut+u.lastOut
		snapshot.InputTokens, snapshot.OutputTokens = &inputTotal, &outputTotal
	}
	if cost != nil {
		total := u.baseCost + u.lastCost
		snapshot.CostUSD = &total
	}
	return snapshot
}
