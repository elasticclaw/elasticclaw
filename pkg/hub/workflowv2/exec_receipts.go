package workflowv2

import (
	"context"
	"encoding/json"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// ApplyCommandReceipt processes a claw-to-hub completion/failure event for an
// exec.run or dependency.update command. It parses the typed receipt, projects
// the protected exec.* fact namespace, and applies the event as a hub/engine
// event so the workflow can react to it without trusting the claw as the fact
// owner.
func (s *Store) ApplyCommandReceipt(ctx context.Context, envelope typesv2.ControlEnvelope) (typesv2.ControlReceipt, error) {
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionClawToHub); err != nil {
		return typesv2.ControlReceipt{MessageID: envelope.MessageID,
			Disposition: typesv2.DispositionRejected, Reason: err.Error()}, err
	}

	var payload map[string]interface{}
	if len(envelope.Payload) > 0 {
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return typesv2.ControlReceipt{MessageID: envelope.MessageID,
				Disposition: typesv2.DispositionRejected, Reason: err.Error()}, err
		}
	}

	facts := commandReceiptFacts(envelope.Kind, payload)
	now := s.now().UTC()
	result, err := s.ApplyEvent(ctx, envelope.RunID, EventInput{
		ID:                   envelope.MessageID,
		MessageID:            envelope.MessageID,
		Kind:                 string(envelope.Kind),
		AttemptID:            envelope.AttemptID,
		ExpectedStateVersion: envelope.ExpectedStateVersion,
		Producer:             ProducerEngine,
		Provenance:           typesv2.EvidenceProvenance{Producer: string(ProducerEngine), ObservedAt: now},
		Facts:                facts,
	})
	if err != nil {
		return typesv2.ControlReceipt{MessageID: envelope.MessageID,
			Disposition: typesv2.DispositionRejected, Reason: err.Error()}, err
	}
	return typesv2.ControlReceipt{
		MessageID:    envelope.MessageID,
		Disposition:  result.Disposition,
		StateVersion: result.Run.StateVersion,
		Reason:       result.Reason,
	}, nil
}

func commandReceiptFacts(kind typesv2.ControlMessageKind, receipt map[string]interface{}) map[string]interface{} {
	facts := map[string]interface{}{}
	setIfPresent := func(key string) {
		if v, ok := receipt[key]; ok {
			facts["exec."+key] = v
		}
	}

	switch kind {
	case typesv2.MessageExecRunCompleted, typesv2.MessageExecRunFailed:
		for _, key := range []string{"exit_code", "succeeded", "stdout", "stderr", "error"} {
			setIfPresent(key)
		}
		// Keep the most recent run under a stable key so transitions can read
		// exec.last_run.* without tracking attempt IDs.
		if nested, ok := facts["exec.succeeded"]; ok {
			facts["exec.last_run.succeeded"] = nested
			delete(facts, "exec.succeeded")
		}
		if exit, ok := facts["exec.exit_code"]; ok {
			facts["exec.last_run.exit_code"] = exit
			delete(facts, "exec.exit_code")
		}
		if out, ok := facts["exec.stdout"]; ok {
			facts["exec.last_run.stdout"] = out
			delete(facts, "exec.stdout")
		}
		if errOut, ok := facts["exec.stderr"]; ok {
			facts["exec.last_run.stderr"] = errOut
			delete(facts, "exec.stderr")
		}
		if errText, ok := facts["exec.error"]; ok {
			facts["exec.last_run.error"] = errText
			delete(facts, "exec.error")
		}
	case typesv2.MessageDependencyUpdateCompleted, typesv2.MessageDependencyUpdateFailed:
		for _, key := range []string{"ecosystems", "manifests", "updates", "commands", "files_changed", "succeeded", "error"} {
			setIfPresent(key)
		}
		if v, ok := receipt["succeeded"]; ok {
			facts["exec.dependency_update.succeeded"] = v
		}
		if v, ok := receipt["error"]; ok {
			facts["exec.dependency_update.error"] = v
		}
		if v, ok := receipt["files_changed"]; ok {
			facts["exec.dependency_update.files_changed"] = v
		}
		if v, ok := receipt["ecosystems"]; ok {
			facts["exec.dependency_update.ecosystems"] = v
		}
		if v, ok := receipt["manifests"]; ok {
			facts["exec.dependency_update.manifests"] = v
		}
		if v, ok := receipt["updates"]; ok {
			facts["exec.dependency_update.updates"] = v
		}
		if v, ok := receipt["commands"]; ok {
			facts["exec.dependency_update.commands"] = v
		}
	default:
		// Unknown command receipt kinds should not reach this helper.
	}
	return facts
}

