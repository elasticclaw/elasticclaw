package pipeline

import "gopkg.in/yaml.v3"

// Pipeline is the parsed representation of a factory's pipeline_yaml field.
type Pipeline struct {
	Stages []Stage `yaml:"stages"`
}

// Stage represents a single stage in the pipeline.
type Stage struct {
	ID       string    `yaml:"id"`
	Label    string    `yaml:"label"`
	Entry    bool      `yaml:"entry"`
	Terminal bool      `yaml:"terminal"`
	Triggers []Trigger `yaml:"triggers"`
	OnEnter  OnEnter   `yaml:"on_enter"`
}

// Trigger defines a condition that causes a transition into the parent stage.
// Exactly one field should be set.
type Trigger struct {
	// MessageContains matches when a claw message contains this substring.
	MessageContains string `yaml:"message_contains"`
	// PRMerged is true when the pr_merged key is present in the YAML (even with null value).
	PRMerged bool
	// PRClosed is true when the pr_closed key is present in the YAML (even with null value).
	PRClosed bool
	// PRConditions transitions when all stated PR conditions are met.
	PRConditions *PRConditionsTrigger `yaml:"pr_conditions"`
}

// PRConditionsTrigger specifies compound PR state conditions that must all pass.
type PRConditionsTrigger struct {
	CI       string `yaml:"ci"`        // "passing" — all check runs must be success/skipped
	Reviews  string `yaml:"reviews"`   // "clean" — no CHANGES_REQUESTED reviews
	QuietFor string `yaml:"quiet_for"` // e.g. "1h", "30m" — optional quiet period since last comment
}

// UnmarshalYAML implements yaml.Unmarshaler so that bare `pr_merged:` and
// `pr_closed:` keys (which have a null/empty value in YAML) are treated as
// true — i.e. key presence alone activates the trigger.
func (t *Trigger) UnmarshalYAML(value *yaml.Node) error {
	// value is a mapping node for each trigger entry
	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		val := value.Content[i+1]
		switch key {
		case "message_contains":
			t.MessageContains = val.Value
		case "pr_merged":
			// Presence of the key (even with null/empty/false value) means true
			t.PRMerged = true
		case "pr_closed":
			t.PRClosed = true
		case "pr_conditions":
			// Decode the pr_conditions sub-mapping
			var cond PRConditionsTrigger
			if val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					subKey := val.Content[j].Value
					subVal := val.Content[j+1].Value
					switch subKey {
					case "ci":
						cond.CI = subVal
					case "reviews":
						cond.Reviews = subVal
					case "quiet_for":
						cond.QuietFor = subVal
					}
				}
			}
			t.PRConditions = &cond
		}
	}
	return nil
}

// MoveIssueAction specifies the target status and optional explicit issue ID
// for the move_issue pipeline action. If IssueID is empty, the issue is looked
// up from the claw's factory tracking (webhook issue or manual trigger inputs).
type MoveIssueAction struct {
	Status  string `yaml:"status"`
	IssueID string `yaml:"issue_id,omitempty"`
}

// OnEnter holds the actions to run when entering a stage.
type OnEnter struct {
	// Inject sends this message to the claw as a user message.
	Inject string `yaml:"inject"`
	// MoveIssue moves the associated Linear/Shortcut issue to this status name.
	MoveIssue MoveIssueAction `yaml:"move_issue"`
	// MergePR triggers the GitHub merge API for the tracked PR (stub — not yet implemented).
	MergePR bool `yaml:"merge_pr,omitempty"`
	// CloseIssue closes the associated GitHub issue when entering this stage.
	CloseIssue bool `yaml:"close_issue,omitempty"`
	// AddLabels adds the specified labels to the associated GitHub issue.
	AddLabels []string `yaml:"add_labels,omitempty"`
	// RemoveLabels removes the specified labels from the associated GitHub issue.
	RemoveLabels []string `yaml:"remove_labels,omitempty"`
}

// UnmarshalYAML implements yaml.Unmarshaler for OnEnter so that move_issue can
// be specified either as a scalar string (backward compat) or as a mapping.
func (oe *OnEnter) UnmarshalYAML(value *yaml.Node) error {
	// Use a shadow type to avoid infinite recursion.
	type rawOnEnter struct {
		Inject       string            `yaml:"inject"`
		MoveIssueRaw yaml.Node          `yaml:"move_issue"`
		MergePR      bool              `yaml:"merge_pr,omitempty"`
		CloseIssue   bool              `yaml:"close_issue,omitempty"`
		AddLabels    []string          `yaml:"add_labels,omitempty"`
		RemoveLabels []string          `yaml:"remove_labels,omitempty"`
	}
	var raw rawOnEnter
	if err := value.Decode(&raw); err != nil {
		return err
	}

	oe.Inject = raw.Inject
	oe.MergePR = raw.MergePR
	oe.CloseIssue = raw.CloseIssue
	oe.AddLabels = raw.AddLabels
	oe.RemoveLabels = raw.RemoveLabels

	if raw.MoveIssueRaw.Kind == 0 {
		// move_issue not present
		return nil
	}

	if raw.MoveIssueRaw.Kind == yaml.ScalarNode {
		// Bare string: treat as status, no explicit issue_id
		oe.MoveIssue = MoveIssueAction{Status: raw.MoveIssueRaw.Value}
	} else if raw.MoveIssueRaw.Kind == yaml.MappingNode {
		var mia MoveIssueAction
		if err := raw.MoveIssueRaw.Decode(&mia); err != nil {
			return err
		}
		oe.MoveIssue = mia
	}
	return nil
}

// Parse decodes YAML bytes into a Pipeline. Returns an error if the YAML is invalid.
func Parse(yamlBytes []byte) (*Pipeline, error) {
	var p Pipeline
	if err := yaml.Unmarshal(yamlBytes, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// EntryStage returns the first stage marked entry:true, or nil if none.
func (p *Pipeline) EntryStage() *Stage {
	for i := range p.Stages {
		if p.Stages[i].Entry {
			return &p.Stages[i]
		}
	}
	return nil
}

// StageForMessageContains returns the first stage that has a message_contains
// trigger matching the given message text. Returns nil if none match.
func (p *Pipeline) StageForMessageContains(message string) *Stage {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.MessageContains != "" && containsFold(message, t.MessageContains) {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// StageForPRMerged returns the first stage with a pr_merged trigger, or nil.
func (p *Pipeline) StageForPRMerged() *Stage {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.PRMerged {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// StageForPRClosed returns the first stage with a pr_closed trigger, or nil.
func (p *Pipeline) StageForPRClosed() *Stage {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.PRClosed {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// StageForPRConditions returns the first stage whose trigger has a non-nil PRConditions, or nil.
func (p *Pipeline) StageForPRConditions() *Stage {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.PRConditions != nil {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// containsFold reports whether s contains substr, case-insensitively.
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	sl := len(s)
	subl := len(substr)
	if subl > sl {
		return false
	}
	for i := 0; i <= sl-subl; i++ {
		if equalFold(s[i:i+subl], substr) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca == cb {
			continue
		}
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
