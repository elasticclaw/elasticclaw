package hub

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/google/uuid"
)

const (
	workflowV2EffectLease        = time.Minute
	workflowV2EffectRetry        = 5 * time.Second
	workflowV2TaskHeartbeat      = 2 * time.Minute
	workflowV2TaskDeadline       = 2 * time.Hour
	workflowV2EffectPoll         = 250 * time.Millisecond
	workflowV2TaskExpirationPoll = 30 * time.Second
)

func (s *Server) runWorkflowV2Worker(ctx context.Context) {
	worker := "hub-workflow-v2-" + uuid.NewString()
	effectTicker := time.NewTicker(workflowV2EffectPoll)
	expirationTicker := time.NewTicker(workflowV2TaskExpirationPoll)
	defer effectTicker.Stop()
	defer expirationTicker.Stop()
	for {
		if err := s.drainWorkflowV2Effects(ctx, worker); err != nil && ctx.Err() == nil {
			log.Printf("[workflow-v2] effect worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-expirationTicker.C:
			if _, err := workflowv2.NewStore(s.db).ExpireAgentTasks(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[workflow-v2] expire agent tasks: %v", err)
			}
		case <-effectTicker.C:
		}
	}
}

func (s *Server) drainWorkflowV2Effects(ctx context.Context, worker string) error {
	store := workflowv2.NewStore(s.db)
	for i := 0; i < 100 && ctx.Err() == nil; i++ {
		claim, err := store.ClaimEffect(ctx, worker, workflowV2EffectLease)
		if err != nil {
			return err
		}
		if claim == nil {
			return nil
		}
		if err := s.executeWorkflowV2Effect(ctx, store, worker, *claim); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Server) executeWorkflowV2Effect(ctx context.Context, store *workflowv2.Store,
	worker string, claim workflowv2.EffectClaim) error {
	switch claim.Effect.Kind {
	case "agent.task":
		if _, err := store.MaterializeAgentTask(ctx, claim.Effect.ID, claim.AttemptID, worker,
			workflowV2TaskHeartbeat, workflowV2TaskDeadline); err == nil {
			return nil
		} else {
			return store.CompleteEffect(ctx, workflowv2.CompleteEffectRequest{
				EffectID: claim.Effect.ID, AttemptID: claim.AttemptID, Worker: worker,
				Status: workflowv2.EffectRetryableFailed, Error: err.Error(), RetryAfter: workflowV2EffectRetry,
			})
		}
	default:
		err := fmt.Errorf("hub has no workflow v2 executor for effect kind %q", claim.Effect.Kind)
		return store.CompleteEffect(ctx, workflowv2.CompleteEffectRequest{
			EffectID: claim.Effect.ID, AttemptID: claim.AttemptID, Worker: worker,
			Status: workflowv2.EffectPermanentFailed, Error: err.Error(),
		})
	}
}
