package v2

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Protected fact namespaces that workflow-authored assert/set cannot write.
var protectedNamespaces = []string{
	"ci.",
	"pull_request.",
	"review.",
	"effects.",
	"workflow.",
	"operator.",
}

// Writable namespaces for workflow-authored facts.
var writableNamespaces = []string{
	"work.",
	"custom.",
}

var knownWorkflowKeys = map[string]bool{
	"schema_version": true,
	"name":           true,
	"enabled":        true,
	"initial_state":  true,
	"states":         true,
	"transitions":    true,
	"commands":       true,
	"ci":             true,
	"review":         true,
	"delivery":       true,
	"events":         true,
}

// ParseWorkflow unmarshals workflow v2 YAML. It does not validate.
func ParseWorkflow(data []byte) (*Workflow, error) {
	version, err := DetectSchemaVersion(data)
	if err != nil {
		return nil, err
	}
	if !IsV2(version) {
		return nil, fmt.Errorf("workflow schema_version %q is not v2 (want 2 or v2)", version)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	for key := range raw {
		if !knownWorkflowKeys[key] {
			return nil, fmt.Errorf("workflow: unknown field %q", key)
		}
	}

	var wf Workflow
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&wf); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	wf.Raw = raw
	return &wf, nil
}

// ValidateWorkflow structurally validates a workflow v2 document (without
// workspace pairing). Pair validation is ValidateWorkflowAgainstWorkspace.
func ValidateWorkflow(wf *Workflow) (*ResolvedWorkflow, error) {
	if wf == nil {
		return nil, fmt.Errorf("workflow is nil")
	}
	if !IsV2(SchemaVersionString(wf.SchemaVersion)) {
		return nil, fmt.Errorf("workflow schema_version %q is not v2", SchemaVersionString(wf.SchemaVersion))
	}
	if strings.TrimSpace(wf.Name) == "" {
		return nil, fmt.Errorf("workflow name is required")
	}
	if strings.TrimSpace(wf.InitialState) == "" {
		return nil, fmt.Errorf("workflow %q: initial_state is required", wf.Name)
	}
	if len(wf.States) == 0 {
		return nil, fmt.Errorf("workflow %q: states is required", wf.Name)
	}
	if _, ok := wf.States[wf.InitialState]; !ok {
		return nil, fmt.Errorf("workflow %q: initial_state %q is not defined in states", wf.Name, wf.InitialState)
	}

	for name, st := range wf.States {
		if err := validateResourceName("states", name); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		if st.OnEnter != nil {
			if err := validateFactWrites(fmt.Sprintf("states.%s.on_enter", name), st.OnEnter.Assert, st.OnEnter.Set); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			if err := validateEffectsShape(fmt.Sprintf("states.%s.on_enter.effects", name), st.OnEnter.Effects); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
		if st.Phase != "" && !IsDisplayPhase(st.Phase) {
			return nil, fmt.Errorf("workflow %q: states.%s.phase %q is unsupported", wf.Name, name, st.Phase)
		}
		if wf.Enabled && st.Phase == "" {
			return nil, fmt.Errorf("workflow %q: states.%s.phase is required when the workflow is enabled", wf.Name, name)
		}
		if err := validateNoTranscriptFacts(fmt.Sprintf("states.%s.invariant", name), st.Invariant); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
	}

	// Transitions form legal graph edges.
	for name, tr := range wf.Transitions {
		if err := validateResourceName("transitions", name); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		fromStates, err := FromStates(tr.From)
		if err != nil {
			return nil, fmt.Errorf("workflow %q: transitions.%s.from: %w", wf.Name, name, err)
		}
		for _, fs := range fromStates {
			st, ok := wf.States[fs]
			if !ok {
				return nil, fmt.Errorf("workflow %q: transitions.%s.from %q: unknown state", wf.Name, name, fs)
			}
			if st.Terminal {
				return nil, fmt.Errorf("workflow %q: transitions.%s.from %q: terminal states cannot have outgoing transitions", wf.Name, name, fs)
			}
		}
		if strings.TrimSpace(tr.To) == "" {
			return nil, fmt.Errorf("workflow %q: transitions.%s.to is required", wf.Name, name)
		}
		if _, ok := wf.States[tr.To]; !ok {
			return nil, fmt.Errorf("workflow %q: transitions.%s.to %q: unknown state", wf.Name, name, tr.To)
		}
		if isTranscriptEvent(tr.On) {
			return nil, fmt.Errorf("workflow %q: transitions.%s.on %q: conversation/transcript events cannot control workflow v2", wf.Name, name, tr.On)
		}
		if err := validateNoTranscriptFacts(fmt.Sprintf("transitions.%s.when", name), tr.When); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		if err := ValidatePredicateTree(fmt.Sprintf("transitions.%s.when", name), tr.When); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		if err := validateFactWrites(fmt.Sprintf("transitions.%s", name), tr.Assert, tr.Set); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		if err := validateEffectsShape(fmt.Sprintf("transitions.%s.effects", name), tr.Effects); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
	}

	// Outgoing transition uniqueness: for same from+on, when clauses must not overlap.
	if err := validateTransitionOverlaps(wf); err != nil {
		return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
	}

	// Commands are also legal graph edges.
	for name, cmd := range wf.Commands {
		if err := validateResourceName("commands", name); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		fromStates, err := FromStates(cmd.From)
		if err != nil {
			return nil, fmt.Errorf("workflow %q: commands.%s.from: %w", wf.Name, name, err)
		}
		for _, fs := range fromStates {
			if _, ok := wf.States[fs]; !ok {
				return nil, fmt.Errorf("workflow %q: commands.%s.from %q: unknown state", wf.Name, name, fs)
			}
			// Commands may leave non-terminal states; terminal→anything is odd but
			// cancel-from-terminal is not needed. Still allow from terminal? RFC
			// says legal graph includes command edges; forbid terminal from for consistency.
			if wf.States[fs].Terminal {
				return nil, fmt.Errorf("workflow %q: commands.%s.from %q: terminal states cannot have outgoing transitions", wf.Name, name, fs)
			}
		}
		if strings.TrimSpace(cmd.To) == "" {
			return nil, fmt.Errorf("workflow %q: commands.%s.to is required", wf.Name, name)
		}
		if _, ok := wf.States[cmd.To]; !ok {
			return nil, fmt.Errorf("workflow %q: commands.%s.to %q: unknown state", wf.Name, name, cmd.To)
		}
	}

	// Event clauses.
	for eventName, def := range wf.Events {
		if isTranscriptEvent(eventName) {
			return nil, fmt.Errorf("workflow %q: event %q: conversation/transcript events cannot control workflow v2", wf.Name, eventName)
		}
		for i, clause := range def.Clauses {
			path := fmt.Sprintf("events.%s.clauses[%d]", eventName, i)
			// Require state scope so event handling remains deterministic.
			fromStates, err := FromStates(clause.From)
			if err != nil {
				return nil, fmt.Errorf("workflow %q: %s.from: %w (from is required)", wf.Name, path, err)
			}
			for _, fs := range fromStates {
				if _, ok := wf.States[fs]; !ok {
					return nil, fmt.Errorf("workflow %q: %s.from %q: unknown state", wf.Name, path, fs)
				}
			}
			if err := ValidatePredicateTree(path+".when", clause.When); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			if err := validateNoTranscriptFacts(path+".when", clause.When); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			if err := validateFactWrites(path, clause.Assert, clause.Set); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			if err := validateEffectsShape(path+".effects", clause.Effects); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
		if err := validateEventClauseOverlaps(wf.Name, eventName, def.Clauses); err != nil {
			return nil, err
		}
	}

	// Policy structure: minimal name checks; resource refs checked in pair validation.
	if wf.CI != nil {
		for name, policy := range wf.CI.Policies {
			if err := validateResourceName("ci.policies", name); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			if err := validateEvidencePolicy("ci.policies."+name, policy, "ci"); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
	}
	if wf.Review != nil {
		for name, policy := range wf.Review.Policies {
			if err := validateResourceName("review.policies", name); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			if err := validateEvidencePolicy("review.policies."+name, policy, "review"); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
	}
	if err := validateDelivery(wf); err != nil {
		return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
	}

	rev, err := RevisionOf(wf)
	if err != nil {
		return nil, err
	}
	return &ResolvedWorkflow{Workflow: wf, Revision: rev}, nil
}

func validateEvidencePolicy(path string, policy Policy, mode string) error {
	return validateEvidencePolicyNode(path, map[string]interface{}(policy), mode, true)
}

func validateEvidencePolicyNode(path string, node interface{}, mode string, root bool) error {
	value, ok := evidencePolicyMap(node)
	if !ok || len(value) == 0 {
		return fmt.Errorf("%s must be a non-empty policy object", path)
	}
	operator := ""
	for _, candidate := range []string{"all", "any", "not"} {
		if _, exists := value[candidate]; exists {
			if operator != "" {
				return fmt.Errorf("%s cannot combine policy operators %q and %q", path, operator, candidate)
			}
			operator = candidate
		}
	}
	metadata := map[string]bool{}
	if root && mode == "ci" {
		metadata["satisfied_for"] = true
	}
	if root && mode == "review" {
		metadata["invalidate_on_new_head"] = true
	}
	if raw, exists := value["satisfied_for"]; exists {
		if mode != "ci" || raw != "current_pr_head" {
			return fmt.Errorf("%s.satisfied_for must be %q", path, "current_pr_head")
		}
	}
	if raw, exists := value["invalidate_on_new_head"]; exists {
		invalidate, ok := raw.(bool)
		if mode != "review" || !ok || !invalidate {
			return fmt.Errorf("%s.invalidate_on_new_head must be true", path)
		}
	}
	if operator != "" {
		for key := range value {
			if key != operator && !metadata[key] {
				return fmt.Errorf("%s: unsupported field %q beside %s", path, key, operator)
			}
		}
		switch operator {
		case "all", "any":
			items, ok := value[operator].([]interface{})
			if !ok || len(items) == 0 {
				return fmt.Errorf("%s.%s must be a non-empty list", path, operator)
			}
			for i, item := range items {
				if err := validateEvidencePolicyNode(fmt.Sprintf("%s.%s[%d]", path, operator, i), item, mode, false); err != nil {
					return err
				}
			}
		case "not":
			if err := validateEvidencePolicyNode(path+".not", value["not"], mode, false); err != nil {
				return err
			}
		}
		return nil
	}
	if mode == "ci" {
		for key := range value {
			if key != "pipeline" && key != "checks" && !metadata[key] {
				return fmt.Errorf("%s: unsupported CI policy field %q", path, key)
			}
		}
		pipeline, _ := value["pipeline"].(string)
		if strings.TrimSpace(pipeline) == "" {
			return fmt.Errorf("%s.pipeline is required", path)
		}
		checks, ok := value["checks"].([]interface{})
		if !ok || len(checks) == 0 {
			return fmt.Errorf("%s.checks must be a non-empty list", path)
		}
		for i, check := range checks {
			if name, ok := check.(string); !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("%s.checks[%d] must be a non-empty string", path, i)
			}
		}
		return nil
	}
	for key := range value {
		if key != "connection" && key != "approvals" && !metadata[key] {
			return fmt.Errorf("%s: unsupported review policy field %q", path, key)
		}
	}
	connection, _ := value["connection"].(string)
	if strings.TrimSpace(connection) == "" {
		return fmt.Errorf("%s.connection is required", path)
	}
	approvals, ok := evidencePolicyMap(value["approvals"])
	if !ok {
		return fmt.Errorf("%s.approvals is required", path)
	}
	for key := range approvals {
		if key != "minimum" {
			return fmt.Errorf("%s.approvals: unsupported field %q", path, key)
		}
	}
	minimum, ok := policyNonNegativeInteger(approvals["minimum"])
	if !ok || minimum < 0 {
		return fmt.Errorf("%s.approvals.minimum must be a non-negative integer", path)
	}
	return nil
}

func evidencePolicyMap(value interface{}) (map[string]interface{}, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		return typed, true
	case Policy:
		return map[string]interface{}(typed), true
	default:
		return nil, false
	}
}

func policyNonNegativeInteger(value interface{}) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, number >= 0
	case int64:
		return int(number), number >= 0
	case uint64:
		return int(number), true
	case float64:
		integer := int(number)
		return integer, number >= 0 && number == float64(integer)
	default:
		return 0, false
	}
}

func isTranscriptEvent(event string) bool {
	event = strings.ToLower(strings.TrimSpace(event))
	for _, prefix := range []string{"message.", "conversation.", "transcript.", "chat.", "agent.message", "claw.message"} {
		if strings.HasPrefix(event, prefix) {
			return true
		}
	}
	return false
}

func validateNoTranscriptFacts(path string, node interface{}) error {
	return walkNoTranscriptFacts(path, "", node)
}

func walkNoTranscriptFacts(path, prefix string, node interface{}) error {
	switch value := node.(type) {
	case map[string]interface{}:
		for key, child := range value {
			fact := key
			if prefix != "" {
				fact = prefix + "." + key
			}
			if isTranscriptFact(key) || isTranscriptFact(fact) {
				return fmt.Errorf("%s: conversation/transcript fact %q cannot control workflow v2", path, fact)
			}
			if err := walkNoTranscriptFacts(path, fact, child); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range value {
			if err := walkNoTranscriptFacts(path, prefix, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func isTranscriptFact(fact string) bool {
	fact = strings.ToLower(strings.TrimSpace(fact))
	for _, prefix := range []string{"message", "conversation", "transcript", "chat", "agent.message", "claw.message"} {
		if fact == prefix || strings.HasPrefix(fact, prefix+".") {
			return true
		}
	}
	return false
}

func validateDelivery(wf *Workflow) error {
	if wf.Delivery == nil || wf.Delivery.PullRequests == nil {
		return nil
	}
	pr := wf.Delivery.PullRequests
	if pr.Minimum < 0 {
		return fmt.Errorf("delivery.pull_requests.minimum cannot be negative")
	}
	if pr.Required && pr.Minimum == 0 {
		return fmt.Errorf("delivery.pull_requests.minimum must be at least 1 when required")
	}
	if pr.Completion != "" && pr.Completion != DeliveryCompletionAllMerged {
		return fmt.Errorf("delivery.pull_requests.completion %q is unsupported", pr.Completion)
	}
	if pr.CIPolicy != "" {
		if wf.CI == nil {
			return fmt.Errorf("delivery.pull_requests.ci_policy %q: workflow has no ci policies", pr.CIPolicy)
		}
		if _, ok := wf.CI.Policies[pr.CIPolicy]; !ok {
			return fmt.Errorf("delivery.pull_requests.ci_policy %q: unknown ci policy", pr.CIPolicy)
		}
	}
	if pr.ReviewPolicy != "" {
		if wf.Review == nil {
			return fmt.Errorf("delivery.pull_requests.review_policy %q: workflow has no review policies", pr.ReviewPolicy)
		}
		if _, ok := wf.Review.Policies[pr.ReviewPolicy]; !ok {
			return fmt.Errorf("delivery.pull_requests.review_policy %q: unknown review policy", pr.ReviewPolicy)
		}
	}
	return nil
}

// ValidateWorkflowAgainstWorkspace pair-validates a workflow against a resolved workspace.
func ValidateWorkflowAgainstWorkspace(wf *Workflow, rws *ResolvedWorkspace) (*ResolvedWorkflow, error) {
	resolved, err := ValidateWorkflow(wf)
	if err != nil {
		return nil, err
	}
	if rws == nil || rws.Workspace == nil {
		return nil, fmt.Errorf("workflow %q: workspace is required for pair validation", wf.Name)
	}
	ws := rws.Workspace

	// CI policies must reference known pipelines.
	if wf.CI != nil {
		for name, policy := range wf.CI.Policies {
			if err := walkPolicyRefs(fmt.Sprintf("ci.policies.%s", name), policy, ws, "pipeline"); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
	}
	// Review policies must reference known review connections.
	if wf.Review != nil {
		for name, policy := range wf.Review.Policies {
			if err := walkPolicyRefs(fmt.Sprintf("review.policies.%s", name), policy, ws, "connection"); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
	}

	// Effects on transitions, clauses, on_enter: pipeline/connection + capability.
	checkEffects := func(path string, effects []map[string]interface{}) error {
		return validateEffectsAgainstWorkspace(path, effects, rws)
	}
	for name, tr := range wf.Transitions {
		if err := checkEffects(fmt.Sprintf("transitions.%s.effects", name), tr.Effects); err != nil {
			return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
	}
	for sname, st := range wf.States {
		if st.OnEnter != nil {
			if err := checkEffects(fmt.Sprintf("states.%s.on_enter.effects", sname), st.OnEnter.Effects); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
	}
	for eventName, def := range wf.Events {
		for i, clause := range def.Clauses {
			path := fmt.Sprintf("events.%s.clauses[%d].effects", eventName, i)
			if err := checkEffects(path, clause.Effects); err != nil {
				return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
		}
	}

	return resolved, nil
}

// ParseAndValidateWorkflow is the shipped entry point for workflow-only validation.
func ParseAndValidateWorkflow(data []byte) (*ResolvedWorkflow, error) {
	wf, err := ParseWorkflow(data)
	if err != nil {
		return nil, err
	}
	return ValidateWorkflow(wf)
}

// ParseAndValidateWorkflowPair validates workflow YAML against workspace YAML.
func ParseAndValidateWorkflowPair(workflowData, workspaceData []byte) (*ResolvedWorkflow, *ResolvedWorkspace, error) {
	rws, err := ParseAndValidateWorkspace(workspaceData)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace: %w", err)
	}
	wf, err := ParseWorkflow(workflowData)
	if err != nil {
		return nil, rws, err
	}
	rwf, err := ValidateWorkflowAgainstWorkspace(wf, rws)
	if err != nil {
		return nil, rws, err
	}
	return rwf, rws, nil
}

func validateFactWrites(path string, assert, set map[string]interface{}) error {
	for _, facts := range []map[string]interface{}{assert, set} {
		if facts == nil {
			continue
		}
		for key := range facts {
			if err := checkFactKeyWritable(path, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkFactKeyWritable(path, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("%s: empty fact key", path)
	}
	for _, p := range protectedNamespaces {
		if key == strings.TrimSuffix(p, ".") || strings.HasPrefix(key, p) {
			return fmt.Errorf("%s: cannot write protected namespace %q (workflow may only write work.* or custom.*)", path, key)
		}
	}
	writable := false
	for _, p := range writableNamespaces {
		if key == strings.TrimSuffix(p, ".") || strings.HasPrefix(key, p) {
			writable = true
			break
		}
	}
	if !writable {
		// Bare keys without namespace are not allowed for assert/set in v2.
		return fmt.Errorf("%s: fact key %q must be under work.* or custom.*", path, key)
	}
	return nil
}

func validateEffectsShape(path string, effects []map[string]interface{}) error {
	for i, eff := range effects {
		if len(eff) == 0 {
			return fmt.Errorf("%s[%d]: empty effect", path, i)
		}
		// Each effect is a single-key map: { "ci.trigger": { ... } }
		if len(eff) != 1 {
			return fmt.Errorf("%s[%d]: effect must have exactly one operation key", path, i)
		}
	}
	return nil
}

func validateEffectsAgainstWorkspace(path string, effects []map[string]interface{}, rws *ResolvedWorkspace) error {
	ws := rws.Workspace
	for i, eff := range effects {
		for op, raw := range eff {
			epath := fmt.Sprintf("%s[%d].%s", path, i, op)
			cfg, _ := raw.(map[string]interface{})
			switch op {
			case EffectCITrigger, EffectCIRetry, EffectCICancel:
				pipelineName, _ := cfg["pipeline"].(string)
				if strings.TrimSpace(pipelineName) == "" {
					return fmt.Errorf("%s: pipeline is required", epath)
				}
				pipe, ok := ws.CIPipeline(pipelineName)
				if !ok {
					return fmt.Errorf("%s: unknown pipeline %q", epath, pipelineName)
				}
				capNeeded, ok := CapabilityForEffect(op)
				if !ok {
					continue
				}
				caps := rws.ResolvedCICaps[pipe.Connection]
				if caps == nil || !caps[capNeeded] {
					return fmt.Errorf("%s: effect %q is unsupported by pipeline %q (connection %q lacks capability %s)",
						epath, op, pipelineName, pipe.Connection, capNeeded)
				}
			case "issue.comment":
				conn, _ := cfg["connection"].(string)
				if strings.TrimSpace(conn) == "" {
					return fmt.Errorf("%s: connection is required", epath)
				}
				if !ws.HasIssueTrackerConnection(conn) {
					return fmt.Errorf("%s: unknown issue_tracker connection %q", epath, conn)
				}
			case "agent.task":
				prompt, _ := cfg["prompt"].(string)
				instructions, _ := cfg["instructions"].(string)
				if strings.TrimSpace(prompt) == "" && strings.TrimSpace(instructions) == "" {
					return fmt.Errorf("%s: prompt or instructions is required", epath)
				}
				if rawFacts, exists := cfg["include_facts"]; exists {
					facts, ok := rawFacts.([]interface{})
					if !ok || len(facts) == 0 || len(facts) > 20 {
						return fmt.Errorf("%s.include_facts must be a list of 1 to 20 fact keys", epath)
					}
					seen := map[string]bool{}
					for index, rawFact := range facts {
						fact, ok := rawFact.(string)
						fact = strings.TrimSpace(fact)
						if !ok || fact == "" || seen[fact] {
							return fmt.Errorf("%s.include_facts[%d] must be a unique non-empty fact key", epath, index)
						}
						if isTranscriptFact(fact) {
							return fmt.Errorf("%s.include_facts[%d]: conversation/transcript fact %q cannot control workflow v2", epath, index, fact)
						}
						seen[fact] = true
					}
				}
			default:
				// Unknown effect ops: reject at pair validation so they fail closed.
				return fmt.Errorf("%s: unsupported effect operation %q", epath, op)
			}
		}
	}
	return nil
}

func walkPolicyRefs(path string, node interface{}, ws *Workspace, mode string) error {
	switch n := node.(type) {
	case map[string]interface{}:
		if pipe, ok := n["pipeline"].(string); ok && mode == "pipeline" {
			if !ws.HasCIPipeline(pipe) {
				return fmt.Errorf("%s: unknown pipeline %q", path, pipe)
			}
		}
		if conn, ok := n["connection"].(string); ok && mode == "connection" {
			if !ws.HasReviewSystemConnection(conn) && !ws.HasSourceControlConnection(conn) {
				return fmt.Errorf("%s: unknown review/source_control connection %q", path, conn)
			}
		}
		for k, v := range n {
			if err := walkPolicyRefs(path+"."+k, v, ws, mode); err != nil {
				return err
			}
		}
	case []interface{}:
		for i, item := range n {
			if err := walkPolicyRefs(fmt.Sprintf("%s[%d]", path, i), item, ws, mode); err != nil {
				return err
			}
		}
	case Policy:
		return walkPolicyRefs(path, map[string]interface{}(n), ws, mode)
	}
	return nil
}

func validateTransitionOverlaps(wf *Workflow) error {
	// Group transitions by from-state + on event.
	type key struct {
		from string
		on   string
	}
	type item struct {
		name string
		when map[string]interface{}
	}
	groups := map[key][]item{}
	for name, tr := range wf.Transitions {
		fromStates, err := FromStates(tr.From)
		if err != nil {
			return err
		}
		on := strings.TrimSpace(tr.On)
		for _, fs := range fromStates {
			k := key{from: fs, on: on}
			groups[k] = append(groups[k], item{name: name, when: tr.When})
		}
	}
	for k, items := range groups {
		if len(items) < 2 {
			continue
		}
		// Same from+on with empty on is still ambiguous if both match.
		for i := 0; i < len(items); i++ {
			for j := i + 1; j < len(items); j++ {
				overlap, witness := ClausesOverlap(items[i].when, items[j].when)
				if overlap {
					ctx := fmt.Sprintf("transitions from state %q on %q", k.from, k.on)
					if k.on == "" {
						ctx = fmt.Sprintf("transitions from state %q", k.from)
					}
					return fmt.Errorf("%s", FormatOverlapError(ctx, k.from,
						[]string{"transitions." + items[i].name, "transitions." + items[j].name},
						witness))
				}
			}
		}
	}
	return nil
}

func validateEventClauseOverlaps(workflowName, eventName string, clauses []EventClause) error {
	// Group by from state.
	type item struct {
		index int
		when  map[string]interface{}
	}
	byState := map[string][]item{}
	for i, clause := range clauses {
		fromStates, err := FromStates(clause.From)
		if err != nil {
			return err
		}
		for _, fs := range fromStates {
			byState[fs] = append(byState[fs], item{index: i, when: clause.When})
		}
	}
	for state, items := range byState {
		if len(items) < 2 {
			continue
		}
		for i := 0; i < len(items); i++ {
			for j := i + 1; j < len(items); j++ {
				overlap, witness := ClausesOverlap(items[i].when, items[j].when)
				if overlap {
					ctx := fmt.Sprintf("event %q", eventName)
					paths := []string{
						fmt.Sprintf("events.%s.clauses[%d]", eventName, items[i].index),
						fmt.Sprintf("events.%s.clauses[%d]", eventName, items[j].index),
					}
					return fmt.Errorf("workflow %q: %s", workflowName, FormatOverlapError(ctx, state, paths, witness))
				}
			}
		}
	}
	return nil
}
