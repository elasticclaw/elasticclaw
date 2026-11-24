package workflowv2

import "context"

// MergeabilityDeferKey exposes the mergeability deferred-replay key format to
// external tests so they match the producer's spelling.
var MergeabilityDeferKey = mergeabilityDeferKey

// SettleReplayedInvariant runs the settle that ends a replay, so external tests
// can interleave observations between a replay's queue snapshot and its settle.
func (s *Store) SettleReplayedInvariant(ctx context.Context, runID string) error {
	return s.settleReplayedInvariant(ctx, runID)
}
