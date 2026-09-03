package convert

import (
	"fmt"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"gopkg.in/yaml.v3"
)

func convertWorkflowV1ToV2(data []byte, opts Options) (Result, error) {
	var warnings []string

	var wf types.WorkflowConfig
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return Result{}, fmt.Errorf("parse workflow: %w", err)
	}
	if err := types.NormalizeWorkflowConfig(&wf); err != nil {
		return Result{}, fmt.Errorf("normalize workflow: %w", err)
	}
	name := strings.TrimSpace(wf.Name)
	if name == "" {
		return Result{}, fmt.Errorf("workflow name is required")
	}
	if len(wf.Stages) == 0 {
		return Result{}, fmt.Errorf("workflow %q: v1 stages are required to convert (no stages found)", name)
	}

	// Detect entry / build state list.
	initial := ""
	states := map[string]v2.State{}
	stageOrder := make([]string, 0, len(wf.Stages))
	nonTerminal := []string{}

	for i, st := range wf.Stages {
		id := strings.TrimSpace(st.ID)
		if id == "" {
			return Result{}, fmt.Errorf("workflow %q: stages[%d].id is required", name, i)
		}
		stageOrder = append(stageOrder, id)
		desc := strings.TrimSpace(st.Label)
		state := v2.State{
			Description: desc,
			Terminal:    st.Terminal,
		}
		// Convert on_enter inject → agent.task effect; label mutations → warnings.
		// Terminal states cannot have effects in v2 because the run is marked
		// finished before the on_enter effects would be scheduled.
		actions, onEnterWarns := convertStageOnEnter(id, st.OnEnter)
		if st.Terminal && actions != nil && len(actions.Effects) > 0 {
			appendWarning(&warnings, "states.%s.on_enter: terminal state effects are not supported in v2 and were dropped", id)
			actions = nil
		}
		if actions != nil {
			state.OnEnter = actions
		}
		warnings = append(warnings, onEnterWarns...)
		states[id] = state
		if st.Entry && initial == "" {
			initial = id
		}
		if !st.Terminal {
			nonTerminal = append(nonTerminal, id)
		}
	}
	if initial == "" {
		// Fall back to first non-terminal stage, else first stage.
		if len(nonTerminal) > 0 {
			initial = nonTerminal[0]
			appendWarning(&warnings, "initial_state: no stage marked entry: true; using first non-terminal stage %q", initial)
		} else {
			initial = stageOrder[0]
			appendWarning(&warnings, "initial_state: no entry stage found; using %q", initial)
		}
	}

	transitions := map[string]v2.Transition{}
	usedTransitionNames := map[string]int{}

	for _, st := range wf.Stages {
		id := strings.TrimSpace(st.ID)
		// Preceding non-terminal stages are the conservative `from` set for
		// "enter this stage when trigger fires" v1 semantics.
		fromStates := precedingNonTerminal(stageOrder, states, id)
		if len(fromStates) == 0 {
			// If nothing precedes, allow from other non-terminals except self.
			for _, nt := range nonTerminal {
				if nt != id {
					fromStates = append(fromStates, nt)
				}
			}
		}

		for ti, trig := range st.Triggers {
			path := fmt.Sprintf("stages.%s.triggers[%d]", id, ti)
			mapped, warn := mapV1Trigger(path, trig)
			warnings = append(warnings, warn...)
			if mapped == nil {
				continue
			}
			if len(fromStates) == 0 {
				appendWarning(&warnings, "%s: no source states available for transition to %q; skipped", path, id)
				continue
			}
			tname := uniqueName(fmt.Sprintf("to_%s_via_%s", id, mapped.slug), usedTransitionNames)
			tr := v2.Transition{
				From: stringOrList(fromStates),
				On:   mapped.on,
				To:   id,
			}
			if len(mapped.when) > 0 {
				tr.When = mapped.when
			}
			transitions[tname] = tr
		}
	}

	// Warn about v1 text-control markers that must not become trusted evidence.
	rawStr := string(data)
	for _, marker := range []string{"[DONE]", "[READY_TO_COMMIT]", "message_contains"} {
		if strings.Contains(rawStr, marker) {
			appendWarning(&warnings, "v1 text control %q detected: never reinterpreting as trusted v2 evidence — replace with explicit transitions/event clauses after review", marker)
		}
	}

	// Integration/trigger metadata does not map 1:1; surface for operators.
	if wf.Integration != "" {
		appendWarning(&warnings, "integration %q: v2 workflows are event/state driven; re-bind trigger sources via workspace connections and runtime adapters (not embedded as v1 integration)", wf.Integration)
	}
	if wf.Trigger != nil {
		appendWarning(&warnings, "trigger: v1 trigger block not copied into v2 — configure run creation / issue association outside the v2 state machine (hub trigger adapters)")
	}
	if len(wf.Inputs) > 0 {
		appendWarning(&warnings, "inputs: %d v1 input(s) not represented in workflow v2 schema yet; preserve separately if still needed for manual trigger UX", len(wf.Inputs))
	}
	if len(wf.Volumes) > 0 {
		appendWarning(&warnings, "volumes: %d v1 volume(s) not represented in workflow v2 schema yet", len(wf.Volumes))
	}
	if wf.ConcurrencyGroup != "" {
		appendWarning(&warnings, "concurrency_group %q: not represented in workflow v2 schema yet", wf.ConcurrencyGroup)
	}

	out := v2.Workflow{
		SchemaVersion: 2,
		Name:          name,
		Enabled:       false, // conversion is always an inactive draft
		InitialState:  initial,
		States:        states,
	}
	if len(transitions) > 0 {
		out.Transitions = transitions
	}

	// Validate structurally; pair-validate when workspace YAML provided.
	if shouldValidate(opts) {
		tmp, err := yaml.Marshal(out)
		if err != nil {
			return Result{}, err
		}
		if len(opts.WorkspaceYAML) > 0 {
			wsVer, err := DetectVersion(opts.WorkspaceYAML)
			if err == nil && wsVer == "2" {
				if _, _, err := v2.ParseAndValidateWorkflowPair(tmp, opts.WorkspaceYAML); err != nil {
					return Result{}, fmt.Errorf("converted workflow failed v2 pair validation: %w", err)
				}
			} else {
				if _, err := v2.ParseAndValidateWorkflow(tmp); err != nil {
					return Result{}, fmt.Errorf("converted workflow failed v2 validation: %w", err)
				}
				if wsVer == "v1" {
					appendWarning(&warnings, "workspace document is still v1; convert the workspace first for pair validation of pipelines/connections")
				}
			}
		} else if _, err := v2.ParseAndValidateWorkflow(tmp); err != nil {
			return Result{}, fmt.Errorf("converted workflow failed v2 validation: %w", err)
		}
	}

	encoded, err := yaml.Marshal(out)
	if err != nil {
		return Result{}, fmt.Errorf("marshal workflow v2: %w", err)
	}
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		encoded = append(encoded, '\n')
	}
	sort.Strings(warnings)

	return Result{
		Output:   encoded,
		From:     "v1",
		To:       "2",
		Warnings: warnings,
	}, nil
}

type mappedTrigger struct {
	on   string
	slug string
	when map[string]interface{}
}

func mapV1Trigger(path string, trig map[string]interface{}) (*mappedTrigger, []string) {
	var warnings []string
	if len(trig) == 0 {
		return nil, warnings
	}

	// Deterministic, hub-owned triggers we can map.
	if _, ok := trig["pr_merged"]; ok {
		return &mappedTrigger{
			on:   "pull_request.merged",
			slug: "pr_merged",
			when: map[string]interface{}{
				"pull_request": map[string]interface{}{"state": "merged"},
			},
		}, warnings
	}
	if _, ok := trig["pr_closed"]; ok {
		return &mappedTrigger{
			on:   "pull_request.closed",
			slug: "pr_closed",
			when: map[string]interface{}{
				"pull_request": map[string]interface{}{"state": "closed"},
			},
		}, warnings
	}
	if _, ok := trig["pr_opened"]; ok {
		return &mappedTrigger{
			// V2 learns the claw's dynamic PR set when the authenticated
			// delivery manifest is verified. The GitHub opened webhook normally
			// predates that registration and is therefore not a reliable edge.
			on:   "delivery.verified",
			slug: "pr_opened",
			when: map[string]interface{}{
				"delivery": map[string]interface{}{"open": map[string]interface{}{"not_equals": 0}},
			},
		}, warnings
	}

	// Explicitly reject subjective text controls (RFC migration rules).
	if v, ok := trig["message_contains"]; ok {
		appendWarning(&warnings, "%s: message_contains %v cannot be auto-converted to trusted v2 evidence; add an explicit transition or event clause after choosing the authoritative fact", path, v)
		return nil, warnings
	}
	if v, ok := trig["message_matches"]; ok {
		appendWarning(&warnings, "%s: message_matches %v cannot be auto-converted to trusted v2 evidence", path, v)
		return nil, warnings
	}

	// Unknown trigger keys — warn and skip.
	keys := make([]string, 0, len(trig))
	for k := range trig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	appendWarning(&warnings, "%s: trigger %v not auto-converted; define a v2 transition/event clause manually", path, keys)
	return nil, warnings
}

func convertStageOnEnter(stageID string, onEnter map[string]interface{}) (*v2.StateActions, []string) {
	if len(onEnter) == 0 {
		return nil, nil
	}
	var warnings []string
	actions := &v2.StateActions{}
	has := false

	if inject, ok := onEnter["inject"].(string); ok && strings.TrimSpace(inject) != "" {
		// Agent prompt as an effect — not trusted evidence.
		actions.Effects = append(actions.Effects, map[string]interface{}{
			"agent.task": map[string]interface{}{
				"prompt": inject,
			},
		})
		has = true
		appendWarning(&warnings, "states.%s.on_enter.inject: mapped to agent.task effect (agent prose is never trusted workflow evidence)", stageID)
	}

	if _, ok := onEnter["add_labels"]; ok {
		appendWarning(&warnings, "states.%s.on_enter.add_labels: not auto-converted — express as an issue-tracker effect with an explicit connection after review", stageID)
	}
	if _, ok := onEnter["remove_labels"]; ok {
		appendWarning(&warnings, "states.%s.on_enter.remove_labels: not auto-converted — express as an issue-tracker effect with an explicit connection after review", stageID)
	}
	if _, ok := onEnter["run"]; ok {
		appendWarning(&warnings, "states.%s.on_enter.run: mapped to exec.run effect after review; command output becomes exec.last_run.* facts, not arbitrary trusted data", stageID)
	}
	if _, ok := onEnter["dependency_updates"]; ok {
		appendWarning(&warnings, "states.%s.on_enter.dependency_updates: mapped to dependency.update effect after review; results become exec.dependency_update.* facts", stageID)
	}

	// Surface other keys.
	for k := range onEnter {
		switch k {
		case "inject", "add_labels", "remove_labels", "run", "dependency_updates":
			continue
		default:
			appendWarning(&warnings, "states.%s.on_enter.%s: not auto-converted", stageID, k)
		}
	}

	if !has {
		return nil, warnings
	}
	return actions, warnings
}

func precedingNonTerminal(order []string, states map[string]v2.State, target string) []string {
	var out []string
	for _, id := range order {
		if id == target {
			break
		}
		if st, ok := states[id]; ok && !st.Terminal {
			out = append(out, id)
		}
	}
	return out
}

func stringOrList(states []string) interface{} {
	if len(states) == 1 {
		return states[0]
	}
	// yaml.v3 marshals []string fine; v2.FromStates accepts []interface{} after unmarshal.
	out := make([]interface{}, len(states))
	for i, s := range states {
		out[i] = s
	}
	return out
}

func uniqueName(base string, used map[string]int) string {
	base = nonResourceChars.ReplaceAllString(strings.ToLower(base), "_")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "transition"
	}
	n := used[base]
	used[base] = n + 1
	if n == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, n+1)
}
