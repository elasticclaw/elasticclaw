package pipeline

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	// Gate defines a deterministic review gate evaluated after the stage's
	// on_enter actions complete. The gate inspects a named output (from a
	// prior run action) and produces a pass/fail verdict that can drive
	// automatic stage transitions via gate_result triggers.
	Gate *Gate `yaml:"gate,omitempty"`
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
	// JudgeVerdict transitions when the most recent judge stage produced the
	// specified verdict ("pass" or "fail"). Used for automated retry loops.
	JudgeVerdict string `yaml:"judge_verdict,omitempty"`
	// GateResult transitions when a specific gate stage produced the given
	// verdict ("pass" or "fail"). Verdict is case-insensitive.
	GateResult *GateResultTrigger `yaml:"gate_result,omitempty"`
	// OutputMatches transitions when a named output at a JSON path matches
	// any of the expected values. This is a general primitive for inspecting
	// structured pipeline outputs without gate semantics.
	OutputMatches *OutputMatchesTrigger `yaml:"output_matches,omitempty"`
}

// PRConditionsTrigger specifies compound PR state conditions that must all pass.
type PRConditionsTrigger struct {
	CI       string `yaml:"ci"`        // "passing" — all check runs must be success/skipped
	Reviews  string `yaml:"reviews"`   // "clean" — no CHANGES_REQUESTED reviews
	QuietFor string `yaml:"quiet_for"` // e.g. "1h", "30m" — optional quiet period since last comment
}

// GateResultTrigger transitions when a gate stage produced a specific verdict.
type GateResultTrigger struct {
	Stage   string `yaml:"stage"`   // stage ID of the gate to inspect
	Verdict string `yaml:"verdict"` // "pass" or "fail"
}

// OutputMatchesTrigger transitions when a named output at a JSON path matches
// any of the expected values.
type OutputMatchesTrigger struct {
	Output string   `yaml:"output"` // output name from a prior run action
	Path   string   `yaml:"path"`   // dot-separated JSON path, e.g. "status"
	AnyOf  []string `yaml:"any_of"` // list of values that should trigger a match
}

// GateCondition defines a set of expected values at a JSON path. If the value
// at the path matches any of the expected values, the condition is satisfied.
type GateCondition struct {
	Path   string   `yaml:"path"`   // dot-separated JSON path, e.g. "status"
	Values []string `yaml:"values"` // values that satisfy this condition
}

// Gate defines a deterministic review gate that inspects a named pipeline
// output and produces a pass/fail verdict. Gates are evaluated automatically
// after the stage's on_enter actions complete.
type Gate struct {
	// Output names the pipeline output to inspect. Must match the output name
	// from a prior run action in this or an earlier stage.
	Output string `yaml:"output"`
	// Pass defines the condition that yields a "pass" verdict.
	Pass GateCondition `yaml:"pass,omitempty"`
	// Fail defines the condition that yields a "fail" verdict. If neither pass
	// nor fail match, the verdict is "error".
	Fail GateCondition `yaml:"fail,omitempty"`
	// Required, when true, blocks PR creation if the gate verdict is fail.
	Required bool `yaml:"required,omitempty"`
	// TreatSkippedAsPass, when true, causes a missing/skipped output to be
	// treated as a pass verdict rather than error.
	TreatSkippedAsPass bool `yaml:"treat_skipped_as_pass,omitempty"`
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
		case "judge_verdict":
			t.JudgeVerdict = strings.ToLower(strings.TrimSpace(val.Value))
		case "gate_result":
			var gr GateResultTrigger
			if val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					subKey := val.Content[j].Value
					subVal := val.Content[j+1].Value
					switch subKey {
					case "stage":
						gr.Stage = subVal
					case "verdict":
						gr.Verdict = strings.ToLower(strings.TrimSpace(subVal))
					}
				}
			}
			t.GateResult = &gr
		case "output_matches":
			var om OutputMatchesTrigger
			if val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					subKey := val.Content[j].Value
					subVal := val.Content[j+1]
					switch subKey {
					case "output":
						om.Output = subVal.Value
					case "path":
						om.Path = subVal.Value
					case "any_of":
						if subVal.Kind == yaml.SequenceNode {
							for k := 0; k < len(subVal.Content); k++ {
								om.AnyOf = append(om.AnyOf, subVal.Content[k].Value)
							}
						}
					}
				}
			}
			t.OutputMatches = &om
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

// RunAction executes a shell command in the agent workspace before the
// remaining on_enter actions continue.
type RunAction struct {
	Command         string `yaml:"command"`
	ContinueOnError bool   `yaml:"continue_on_error,omitempty"`
	Timeout         string `yaml:"timeout,omitempty"`
	// Output names the output bucket for this run. When set, stdout is parsed
	// as JSON and the resulting map is stored under that name so later stages
	// can reference values via {{ .Outputs.<name>.<key> }}.
	Output string `yaml:"output,omitempty"`
}

// JudgeInput defines a constrained input source for a judge stage.
type JudgeInput string

const (
	JudgeInputIssue      JudgeInput = "issue"
	JudgeInputGitDiff    JudgeInput = "git_diff"
	JudgeInputTestOutput JudgeInput = "test_output"
	JudgeInputFiles      JudgeInput = "files"
)

// JudgeRequire defines the required verdict for a judge stage to pass.
type JudgeRequire struct {
	Verdict string `yaml:"verdict"`
}

// JudgeAction runs a model-backed review pass with constrained inputs and
// produces a structured verdict. The verdict is stored as a pipeline output
// so later stages can reference it via {{ .Outputs.<name>.verdict }}.
type JudgeAction struct {
	// Model is the LLM model to use for the judge (e.g. "anthropic/claude-sonnet-4-6").
	// If empty, the hub's default model is used.
	Model string `yaml:"model,omitempty"`
	// Inputs lists the bounded inputs to provide to the judge.
	Inputs []JudgeInput `yaml:"inputs"`
	// Require specifies the required verdict for the stage to pass.
	Require JudgeRequire `yaml:"require,omitempty"`
	// Instructions is the system prompt / instructions for the judge.
	Instructions string `yaml:"instructions"`
	// Output names the output bucket for the verdict. When set, the structured
	// judge response is parsed as JSON and stored under that name.
	Output string `yaml:"output,omitempty"`
	// ContinueOnError allows the pipeline to continue even if the judge fails
	// (e.g. for advisory review stages).
	ContinueOnError bool `yaml:"continue_on_error,omitempty"`
	// MaxTokens limits the judge response length.
	MaxTokens int `yaml:"max_tokens,omitempty"`
	// Timeout limits how long the judge call may take (e.g. "2m").
	Timeout string `yaml:"timeout,omitempty"`
}

// OnEnter holds the actions to run when entering a stage.
type OnEnter struct {
	// Run executes a command in the agent workspace.
	Run RunAction `yaml:"run,omitempty"`
	// Judge runs a model-backed review pass with constrained inputs.
	Judge JudgeAction `yaml:"judge,omitempty"`
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
		Run          RunAction   `yaml:"run,omitempty"`
		Judge        JudgeAction `yaml:"judge,omitempty"`
		Inject       string      `yaml:"inject"`
		MoveIssueRaw yaml.Node   `yaml:"move_issue"`
		MergePR      bool        `yaml:"merge_pr,omitempty"`
		CloseIssue   bool        `yaml:"close_issue,omitempty"`
		AddLabels    []string    `yaml:"add_labels,omitempty"`
		RemoveLabels []string    `yaml:"remove_labels,omitempty"`
	}
	var raw rawOnEnter
	if err := value.Decode(&raw); err != nil {
		return err
	}

	oe.Run = raw.Run
	oe.Judge = raw.Judge
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
		return nil, fmt.Errorf("pipeline YAML parse error: %w (hint: template expressions like {{.Issue.Identifier}} must be quoted when used as inline values, e.g. issue_id: \"{{.Issue.Identifier}}\"; literal blocks do not need quoting)", err)
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

// StageForJudgeVerdict returns the first stage that has a judge_verdict trigger
// matching the given verdict ("pass" or "fail"). Returns nil if none match.
func (p *Pipeline) StageForJudgeVerdict(verdict string) *Stage {
	verdict = strings.ToLower(strings.TrimSpace(verdict))
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.JudgeVerdict != "" && strings.EqualFold(t.JudgeVerdict, verdict) {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// StageForGateResult returns the first stage that has a gate_result trigger
// matching the given stage ID and verdict. Returns nil if none match.
func (p *Pipeline) StageForGateResult(stageID, verdict string) *Stage {
	verdict = strings.ToLower(strings.TrimSpace(verdict))
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.GateResult != nil &&
				strings.EqualFold(t.GateResult.Stage, stageID) &&
				strings.EqualFold(t.GateResult.Verdict, verdict) {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// StageForOutputMatches returns the first stage that has an output_matches
// trigger that evaluates to true given the current outputs. Returns nil if
// none match.
func (p *Pipeline) StageForOutputMatches(outputs map[string]map[string]interface{}) *Stage {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.OutputMatches != nil && outputMatches(t.OutputMatches, outputs) {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// outputMatches evaluates an OutputMatchesTrigger against the current outputs.
func outputMatches(om *OutputMatchesTrigger, outputs map[string]map[string]interface{}) bool {
	out, ok := outputs[om.Output]
	if !ok {
		return false
	}
	val := GetJSONPath(out, om.Path)
	if val == nil {
		return false
	}
	strVal, ok := val.(string)
	if !ok {
		return false
	}
	for _, v := range om.AnyOf {
		if strings.EqualFold(strVal, v) {
			return true
		}
	}
	return false
}

// GetJSONPath walks a dot-separated path through a map[string]interface{} and
// returns the value at that path, or nil if the path does not exist.
func GetJSONPath(m map[string]interface{}, path string) interface{} {
	if path == "" {
		return m
	}
	parts := strings.Split(path, ".")
	var current interface{} = m
	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[part]
		case map[interface{}]interface{}:
			current = v[part]
		default:
			return nil
		}
		if current == nil {
			return nil
		}
	}
	return current
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
