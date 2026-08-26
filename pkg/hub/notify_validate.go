package hub

import (
	"fmt"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// notifyActionRef locates one enabled notify action inside a pipeline: the
// stage that declares it, its trimmed "via" notifier name (empty when the
// action has no usable via) and its raw "severity" value.
type notifyActionRef struct {
	StageID  string
	Via      string
	Severity string
}

// notifyActionRefs parses pipelineYAML and returns a ref for every enabled
// notify action. This one walk is shared by author-time validation
// (validateNotifyVias) and the doctor (checkNotifyActions), so the
// two can never disagree about which notify actions a pipeline carries or how
// a "via" is read. An unparseable pipeline yields no refs: a parse failure is
// a different problem surfaced elsewhere (the runner logs it), and
// pipeline.Parse deliberately never fails on a bad notify block.
func notifyActionRefs(pipelineYAML string) []notifyActionRef {
	p, err := pipeline.Parse([]byte(pipelineYAML))
	if err != nil {
		return nil
	}
	var refs []notifyActionRef
	for _, stage := range p.Stages {
		if !stage.OnEnter.Notify.Enabled {
			continue
		}
		refs = append(refs, notifyActionRef{
			StageID:  stage.ID,
			Via:      strings.TrimSpace(stage.OnEnter.Notify.Via),
			Severity: stage.OnEnter.Notify.Severity,
		})
	}
	return refs
}

// effectiveWorkflowPipelineYAML resolves the pipeline the runtime will parse
// for a workflow, without mutating it: the exact resolution
// types.NormalizeWorkflowConfig performs, where stages: is marshalled over
// any directly authored pipeline_yaml:. Loaded workflows are already
// normalized, but push payloads may carry stages: only, so the check must not
// depend on normalization having happened.
func effectiveWorkflowPipelineYAML(workflow *types.WorkflowConfig) (string, error) {
	if workflow == nil {
		return "", nil
	}
	copied := *workflow
	if err := types.NormalizeWorkflowConfig(&copied); err != nil {
		return "", err
	}
	return copied.PipelineYAML, nil
}

// validateNotifyVias rejects a pipeline at save time when it contains a
// notify action whose "via" is blank or names no notifier in notifiers (the
// set under notifications.notifiers in hub.yaml), or whose "severity" is not
// one of the four known values. Every offending action is
// reported in one error, prefixed with label — `workflow "release"`,
// `factory "triage"` — naming its stage and listing the defined notifier
// names so a typo is obvious. It is a free function taking the notifier set
// explicitly so save paths that already hold the server lock (patchSettings)
// can run it without re-entering the lock.
//
// This is deliberately the author-time half of a split with the doctor:
//
//   - Here (every workflow AND factory save path) only the reference is
//     checked, so the author gets immediate blocking feedback while nothing
//     is running yet. Provider settings and secrets are NOT resolved here —
//     they rot independently of the saved artifact and stay the doctor's
//     job.
//   - The doctor's checkNotifyActions re-reads the same refs
//     (notifyActionRefs over the same pipeline YAML) on demand, so a
//     notifier deleted or renamed in hub.yaml AFTER the artifact was saved —
//     which no save-time check can catch — still surfaces with a consistent
//     message, and the referenced notifier is additionally constructed there
//     with real secret resolution under the runtime's secret scope.
//
// Runtime stays lenient on purpose: pipeline.Parse never fails on a bad
// notify block and executeNotifyAction only warns per send, because one
// notification typo must never take a running pipeline down.
func validateNotifyVias(notifiers map[string]types.NotifierConfig, label, pipelineYAML string) error {
	if strings.TrimSpace(pipelineYAML) == "" {
		return nil
	}
	var problems []string
	for _, ref := range notifyActionRefs(pipelineYAML) {
		// Severity is checked here rather than in pipeline.Validate so a
		// typo is rejected while the author is still saving: failing
		// pipeline.Parse instead would let the save through and then kill
		// every stage action of a running pipeline, notify or not.
		if _, ok := notifySeverity(ref.Severity); !ok {
			problems = append(problems, fmt.Sprintf(
				"stage %q has notify severity %q, which must be info, success, warning or error", ref.StageID, ref.Severity))
		}
		if ref.Via == "" {
			problems = append(problems, fmt.Sprintf(
				"stage %q has a notify action without a \"via\" (the name of a notifier under notifications.notifiers)", ref.StageID))
			continue
		}
		nc, ok := notifiers[ref.Via]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"stage %q references notifier %q, which is not configured under notifications.notifiers in hub.yaml", ref.StageID, ref.Via))
			continue
		}
		// The referenced notifier must also be of a registered provider type:
		// ValidateNotificationsConfig only requires a non-blank type, so
		// without this check `notifiers.eng: {type: teams}` plus `via: eng`
		// saves with a 200 and the failure only surfaces at runtime as a
		// per-send warning — defeating the author-time rejection this
		// validation exists for. Provider settings and secrets still stay the
		// doctor's job (they rot independently of the saved artifact).
		if !notify.Supported(nc.Type) {
			problems = append(problems, fmt.Sprintf(
				"stage %q references notifier %q, whose type %q is not a supported notifier type", ref.StageID, ref.Via, nc.Type))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	defined := make([]string, 0, len(notifiers))
	for name := range notifiers {
		defined = append(defined, name)
	}
	sort.Strings(defined)
	definedDesc := "no notifiers are defined in hub.yaml"
	if len(defined) > 0 {
		definedDesc = "defined notifiers: " + strings.Join(defined, ", ")
	}
	return fmt.Errorf("%s notify actions: %s (%s)", label, strings.Join(problems, "; "), definedDesc)
}

// notifierPipelineReferences maps a notifier name to the pipeline stages whose
// notify actions route through it. It walks the same surface the doctor's
// checkNotifyActions judges — every workspace workflow's effective pipeline
// plus every factory's pipeline_yaml — so the two can never disagree about
// which notifiers a pipeline depends on. Factories are passed in rather than
// resolved here because callers already hold the server lock.
func notifierPipelineReferences(factories []*types.FactoryConfig) map[string][]string {
	out := map[string][]string{}
	collect := func(kind, name, pipelineYAML string) {
		for _, ref := range notifyActionRefs(pipelineYAML) {
			if ref.Via == "" {
				continue
			}
			out[ref.Via] = append(out[ref.Via], fmt.Sprintf("%s %q stage %q", kind, name, ref.StageID))
		}
	}
	if workspaces, err := loadExternalWorkspaces(); err == nil {
		for _, workspace := range workspaces {
			if workspace == nil {
				continue
			}
			for _, workflow := range workspace.Workflows {
				if workflow == nil {
					continue
				}
				pipelineYAML, err := effectiveWorkflowPipelineYAML(workflow)
				if err != nil {
					continue
				}
				collect("workflow", workflow.Name, pipelineYAML)
			}
		}
	}
	for _, factory := range factories {
		if factory == nil {
			continue
		}
		collect("factory", factory.Name, factory.PipelineYAML)
	}
	return out
}

// validateNotifierRemovals rejects a settings patch that drops a notifier a
// pipeline notify action still routes through. The Notifier screen lists every
// hub notifier — including ones no lifecycle route uses and only a pipeline
// stage references — and its Remove button sends the whole notifiers map
// without the deleted key. Without this the save returns 200, SaveHubConfig
// writes the notifier out of hub.yaml, and executeNotifyAction then drops
// every stage notification with nothing but a warning in the claw
// conversation.
func validateNotifierRemovals(current, patch *types.NotificationsConfig, factories []*types.FactoryConfig) error {
	if current == nil || patch == nil {
		return nil
	}
	var removed []string
	for name := range current.Notifiers {
		if _, kept := patch.Notifiers[name]; !kept {
			removed = append(removed, name)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	sort.Strings(removed)
	references := notifierPipelineReferences(factories)
	var problems []string
	for _, name := range removed {
		used := references[name]
		if len(used) == 0 {
			continue
		}
		sort.Strings(used)
		problems = append(problems, fmt.Sprintf("%q is still used by %s", name, strings.Join(used, ", ")))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("notifications.notifiers: cannot remove a notifier a pipeline still notifies through: %s", strings.Join(problems, "; "))
}

// configuredNotifiers returns the notifier set defined under
// notifications.notifiers in hub.yaml. It takes the server lock; callers
// already holding it must read s.hubCfg.Notifications directly instead.
func (s *Server) configuredNotifiers() map[string]types.NotifierConfig {
	if cfg := s.notificationsConfig(); cfg != nil {
		return cfg.Notifiers
	}
	return nil
}

// validateWorkflowNotifyVias runs validateNotifyVias over a workflow's
// effective pipeline (the exact resolution the runtime performs, see
// effectiveWorkflowPipelineYAML) on every workflow save path.
func (s *Server) validateWorkflowNotifyVias(workflow *types.WorkflowConfig) error {
	if workflow == nil {
		return nil
	}
	pipelineYAML, err := effectiveWorkflowPipelineYAML(workflow)
	if err != nil {
		// Stages that cannot be marshalled are rejected by the schema
		// validation that runs alongside this check; with no effective
		// pipeline there is nothing notify-shaped to judge.
		return nil
	}
	return validateNotifyVias(s.configuredNotifiers(), fmt.Sprintf("workflow %q", workflow.Name), pipelineYAML)
}

// validateFactoryNotifyVias runs validateNotifyVias over a factory's
// pipeline_yaml. Factories are a notify surface of their own — the runtime
// executes their notify actions via parsePipelineForFactory and the doctor
// judges factory.PipelineYAML as a first-class source — so their save paths
// must apply the same author-time check as workflows.
func (s *Server) validateFactoryNotifyVias(factory *types.FactoryConfig) error {
	if factory == nil {
		return nil
	}
	return validateNotifyVias(s.configuredNotifiers(), fmt.Sprintf("factory %q", factory.Name), factory.PipelineYAML)
}
