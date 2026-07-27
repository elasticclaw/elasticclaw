package v2

import (
	"fmt"
	"strings"
)

// Workflow is the authored workflow v2 document (issue #544).
type Workflow struct {
	SchemaVersion interface{}                `yaml:"schema_version" json:"schema_version"`
	Name          string                     `yaml:"name" json:"name"`
	InitialState  string                     `yaml:"initial_state" json:"initial_state"`
	States        map[string]State           `yaml:"states" json:"states"`
	Transitions   map[string]Transition      `yaml:"transitions,omitempty" json:"transitions,omitempty"`
	Commands      map[string]Command         `yaml:"commands,omitempty" json:"commands,omitempty"`
	CI            *WorkflowCI                `yaml:"ci,omitempty" json:"ci,omitempty"`
	Review        *WorkflowReview            `yaml:"review,omitempty" json:"review,omitempty"`
	Events        map[string]EventDefinition `yaml:"events,omitempty" json:"events,omitempty"`
	Raw           map[string]interface{}     `yaml:"-" json:"-"`
}

// State is an explicit workflow state.
type State struct {
	Description string                 `yaml:"description,omitempty" json:"description,omitempty"`
	Terminal    bool                   `yaml:"terminal,omitempty" json:"terminal,omitempty"`
	Invariant   map[string]interface{} `yaml:"invariant,omitempty" json:"invariant,omitempty"`
	OnEnter     *StateActions          `yaml:"on_enter,omitempty" json:"on_enter,omitempty"`
}

// StateActions are effects/asserts run on state entry.
type StateActions struct {
	Assert  map[string]interface{}   `yaml:"assert,omitempty" json:"assert,omitempty"`
	Effects []map[string]interface{} `yaml:"effects,omitempty" json:"effects,omitempty"`
	Set     map[string]interface{}   `yaml:"set,omitempty" json:"set,omitempty"`
}

// Transition is a named legal edge in the state graph.
type Transition struct {
	From    interface{}              `yaml:"from" json:"from"` // string or []string
	On      string                   `yaml:"on,omitempty" json:"on,omitempty"`
	When    map[string]interface{}   `yaml:"when,omitempty" json:"when,omitempty"`
	To      string                   `yaml:"to" json:"to"`
	Assert  map[string]interface{}   `yaml:"assert,omitempty" json:"assert,omitempty"`
	Set     map[string]interface{}   `yaml:"set,omitempty" json:"set,omitempty"`
	Effects []map[string]interface{} `yaml:"effects,omitempty" json:"effects,omitempty"`
}

// Command is an authenticated operator/request edge.
type Command struct {
	From          interface{} `yaml:"from" json:"from"` // string or []string
	To            string      `yaml:"to" json:"to"`
	RequireReason bool        `yaml:"require_reason,omitempty" json:"require_reason,omitempty"`
}

// WorkflowCI holds CI policies that reference workspace pipelines by name.
type WorkflowCI struct {
	Policies map[string]Policy `yaml:"policies,omitempty" json:"policies,omitempty"`
}

// WorkflowReview holds review policies that reference workspace review connections.
type WorkflowReview struct {
	Policies map[string]Policy `yaml:"policies,omitempty" json:"policies,omitempty"`
}

// Policy is a composable all/any/not policy tree. Leaf nodes reference
// pipeline/connection names; structure is intentionally loose at parse time
// and validated for known resource refs.
type Policy map[string]interface{}

// EventDefinition holds custom event clauses for one event type.
type EventDefinition struct {
	Clauses []EventClause `yaml:"clauses,omitempty" json:"clauses,omitempty"`
}

// EventClause is a state-scoped custom reaction with restricted predicates.
type EventClause struct {
	From    interface{}              `yaml:"from,omitempty" json:"from,omitempty"` // string or []string; required (conservative)
	When    map[string]interface{}   `yaml:"when,omitempty" json:"when,omitempty"`
	Assert  map[string]interface{}   `yaml:"assert,omitempty" json:"assert,omitempty"`
	Set     map[string]interface{}   `yaml:"set,omitempty" json:"set,omitempty"`
	Effects []map[string]interface{} `yaml:"effects,omitempty" json:"effects,omitempty"`
	Ignore  bool                     `yaml:"ignore,omitempty" json:"ignore,omitempty"`
}

// ResolvedWorkflow is a validated workflow plus its content revision.
type ResolvedWorkflow struct {
	Workflow *Workflow
	Revision ContentDigest
}

// FromStates expands a from field (string or []string) into state names.
func FromStates(from interface{}) ([]string, error) {
	switch v := from.(type) {
	case nil:
		return nil, fmt.Errorf("from is required")
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("from cannot be empty")
		}
		return []string{v}, nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("from[%d] must be a non-empty string", i)
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("from cannot be empty")
		}
		return out, nil
	case []string:
		if len(v) == 0 {
			return nil, fmt.Errorf("from cannot be empty")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("from must be a string or list of strings")
	}
}
