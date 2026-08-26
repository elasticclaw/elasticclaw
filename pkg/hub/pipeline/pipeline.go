package pipeline

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pipeline is the parsed representation of a factory's pipeline_yaml field.
type Pipeline struct {
	Stages []Stage `yaml:"stages"`
}

// Stage represents a single stage in the pipeline.
type Stage struct {
	ID         string     `yaml:"id"`
	Label      string     `yaml:"label"`
	Entry      bool       `yaml:"entry"`
	Terminal   bool       `yaml:"terminal"`
	Triggers   []Trigger  `yaml:"triggers"`
	OnEnter    OnEnter    `yaml:"on_enter"`
	SkipIf     *StageSkip `yaml:"skip_if,omitempty"`
	SkipUnless *StageSkip `yaml:"skip_unless,omitempty"`
	// Gate defines a deterministic review gate evaluated after the stage's
	// on_enter actions complete. The gate inspects a named output (from a
	// prior run action) and produces a pass/fail verdict that can drive
	// automatic stage transitions via gate_result triggers.
	Gate *Gate `yaml:"gate,omitempty"`
	// PlanGate marks this stage's gate as the workflow's plan-approval gate.
	// When any stage has plan_gate: true with a configured gate, the hub skips
	// freeform initial-plan keyword approval for claws on this pipeline so
	// plan acceptance is deterministic and not double-enforced.
	PlanGate bool `yaml:"plan_gate,omitempty"`
}

// StageSkip defines a pre-enter stage skip rule. When the condition evaluates
// to true, the runner enters GoTo instead of the current stage.
type StageSkip struct {
	IssueLabels *IssueLabelsSkip `yaml:"issue_labels,omitempty"`
	GoTo        string           `yaml:"go_to"`
}

// IssueLabelsSkip matches when the issue has any configured label.
type IssueLabelsSkip struct {
	Labels []string `yaml:"labels"`
}

// Trigger defines a condition that causes a transition into the parent stage.
// Exactly one field should be set.
type Trigger struct {
	// MessageContains matches when this token opens a line of a claw message,
	// ignoring leading whitespace and markdown decoration. The name is kept for
	// YAML compatibility, but the match is anchored, not a substring scan: see
	// MessageSignals for why. A token quoted mid-sentence does not match.
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
	// StageTimeout transitions when the claw has been in this stage for at least
	// the given duration without another trigger firing first. Evaluated by the
	// reaper, not by per-message trigger evaluation.
	StageTimeout *StageTimeoutTrigger `yaml:"stage_timeout,omitempty"`
}

// StageTimeoutTrigger fires a transition after a stage has been entered for
// at least After, moving the claw to GoTo.
type StageTimeoutTrigger struct {
	After string `yaml:"after"`
	GoTo  string `yaml:"go_to"`
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
		case "stage_timeout":
			var timeout StageTimeoutTrigger
			if val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					switch val.Content[j].Value {
					case "after":
						timeout.After = val.Content[j+1].Value
					case "go_to":
						timeout.GoTo = val.Content[j+1].Value
					}
				}
			}
			t.StageTimeout = &timeout
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

// DependencyUpdatesAction updates repository dependencies using ecosystem-native
// tooling and emits structured JSON output for later workflow stages.
type DependencyUpdatesAction struct {
	Enabled          bool     `yaml:"-" json:"-"`
	Ecosystems       []string `yaml:"ecosystems,omitempty"`
	Paths            []string `yaml:"paths,omitempty"`
	ExcludePaths     []string `yaml:"exclude_paths,omitempty"`
	Grouping         string   `yaml:"grouping,omitempty"`
	IncludeMajor     bool     `yaml:"include_major,omitempty"`
	SeparateMajor    *bool    `yaml:"separate_major,omitempty"`
	SeparateSecurity *bool    `yaml:"separate_security,omitempty"`
	SeparateRuntime  *bool    `yaml:"separate_runtime,omitempty"`
	Allow            []string `yaml:"allow,omitempty"`
	Ignore           []string `yaml:"ignore,omitempty"`
	Output           string   `yaml:"output,omitempty"`
	Timeout          string   `yaml:"timeout,omitempty"`
	ContinueOnError  bool     `yaml:"continue_on_error,omitempty"`
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

// NotifyAction sends a message through a hub-configured notifier
// (notifications.notifiers in hub.yaml: Slack today; email, Google Chat,
// Teams later). Via names the notifier; Text, Subject and Target support the
// same template variables as inject ({{.Issue.*}}, {{.Inputs.*}},
// {{.Outputs.*}}).
//
// A notify block with a missing or blank Via is deliberately NOT a parse
// error. Notifications are advisory by contract, and every caller treats a
// Parse failure as "this claw has no pipeline" — stage transitions, gates and
// PR handling would all silently stop — so one notification typo must never
// take the pipeline down. The action stays Enabled with an empty Via: the
// runner warns loudly in the claw conversation on every attempted send, and
// the doctor's notify-action check flags it before it ever runs.
type NotifyAction struct {
	Enabled bool `yaml:"-" json:"-"`
	// Via is the name of the hub notifier to send through.
	Via string `yaml:"via"`
	// Text is the message body (templated). It is rendered as plain text:
	// providers escape their own markup control characters, because Text
	// interpolates untrusted data (issue titles, command output).
	Text string `yaml:"text"`
	// Subject names what the message is about; templated. It is a semantic
	// field: providers with a native subject line (email) use it there, and
	// every other provider renders it inside its rich layout rather than
	// dropping it — on Slack, the subject gets its own line under the text
	// inside the Block Kit attachment every notify action now renders as.
	Subject string `yaml:"subject,omitempty"`
	// Severity controls provider-native emphasis (the colour stripe). It
	// must be info, success, warning or error when set; the value is
	// rejected at save time (validateNotifyVias), never here, because
	// Parse must stay lenient on notify blocks — a notification typo must
	// not take a whole running pipeline down.
	Severity string `yaml:"severity,omitempty"`
	// Target overrides the notifier's default destination (channel, address);
	// templated.
	Target string `yaml:"target,omitempty"`
	// Options is a provider-specific passthrough (e.g. Slack thread_ts).
	Options map[string]any `yaml:"options,omitempty"`
}

// OnEnter holds the actions to run when entering a stage.
type OnEnter struct {
	// Run executes a command in the agent workspace.
	Run RunAction `yaml:"run,omitempty"`
	// DependencyUpdates updates supported dependency manifests with native package managers.
	DependencyUpdates DependencyUpdatesAction `yaml:"dependency_updates,omitempty"`
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
	// Notify sends a message through a hub-configured notifier.
	Notify NotifyAction `yaml:"notify,omitempty"`
}

// UnmarshalYAML implements yaml.Unmarshaler for OnEnter so that move_issue can
// be specified either as a scalar string (backward compat) or as a mapping.
func (oe *OnEnter) UnmarshalYAML(value *yaml.Node) error {
	// Use a shadow type to avoid infinite recursion.
	type rawOnEnter struct {
		Run                  RunAction   `yaml:"run,omitempty"`
		DependencyUpdatesRaw yaml.Node   `yaml:"dependency_updates"`
		Judge                JudgeAction `yaml:"judge,omitempty"`
		Inject               string      `yaml:"inject"`
		MoveIssueRaw         yaml.Node   `yaml:"move_issue"`
		MergePR              bool        `yaml:"merge_pr,omitempty"`
		CloseIssue           bool        `yaml:"close_issue,omitempty"`
		AddLabels            []string    `yaml:"add_labels,omitempty"`
		RemoveLabels         []string    `yaml:"remove_labels,omitempty"`
		NotifyRaw            yaml.Node   `yaml:"notify"`
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

	if raw.DependencyUpdatesRaw.Kind != 0 {
		var dua DependencyUpdatesAction
		if err := raw.DependencyUpdatesRaw.Decode(&dua); err != nil {
			return err
		}
		dua.Enabled = true
		oe.DependencyUpdates = dua
	}

	if raw.NotifyRaw.Kind != 0 {
		var na NotifyAction
		if err := raw.NotifyRaw.Decode(&na); err != nil {
			return err
		}
		na.Enabled = true
		oe.Notify = na
	}

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
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks pipeline-level invariants needed by the runner.
func (p *Pipeline) Validate() error {
	if p == nil {
		return nil
	}
	stageIDs := make(map[string]bool, len(p.Stages))
	for _, stage := range p.Stages {
		if strings.TrimSpace(stage.ID) != "" {
			stageIDs[stage.ID] = true
		}
	}
	skipEdges := make(map[string][]string)
	for _, stage := range p.Stages {
		for _, trigger := range stage.Triggers {
			if trigger.StageTimeout != nil {
				if err := validateStageTimeoutTrigger(stage.ID, trigger.StageTimeout, stageIDs); err != nil {
					return err
				}
			}
		}
		for _, skip := range []struct {
			name string
			rule *StageSkip
		}{
			{name: "skip_if", rule: stage.SkipIf},
			{name: "skip_unless", rule: stage.SkipUnless},
		} {
			if skip.rule == nil {
				continue
			}
			if err := validateStageSkipRule(stage.ID, skip.name, skip.rule, stageIDs); err != nil {
				return err
			}
			skipEdges[stage.ID] = append(skipEdges[stage.ID], skip.rule.GoTo)
		}
	}
	if cycle := firstSkipCycle(skipEdges); len(cycle) > 0 {
		return fmt.Errorf("pipeline skip cycle detected: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

func validateStageTimeoutTrigger(stageID string, trigger *StageTimeoutTrigger, stageIDs map[string]bool) error {
	after := strings.TrimSpace(trigger.After)
	duration, err := time.ParseDuration(after)
	if err != nil || duration <= 0 {
		return fmt.Errorf("stage %q stage_timeout after must be a positive duration", stageID)
	}
	goTo := strings.TrimSpace(trigger.GoTo)
	if goTo == "" {
		return fmt.Errorf("stage %q stage_timeout go_to is required", stageID)
	}
	if goTo == stageID {
		return fmt.Errorf("stage %q stage_timeout go_to cannot point to itself", stageID)
	}
	if !stageIDs[goTo] {
		return fmt.Errorf("stage %q stage_timeout go_to %q does not reference an existing stage", stageID, goTo)
	}
	return nil
}

func validateStageSkipRule(stageID, name string, rule *StageSkip, stageIDs map[string]bool) error {
	goTo := strings.TrimSpace(rule.GoTo)
	if goTo == "" {
		return fmt.Errorf("stage %q %s go_to is required", stageID, name)
	}
	if goTo == stageID {
		return fmt.Errorf("stage %q %s go_to cannot point to itself", stageID, name)
	}
	if !stageIDs[goTo] {
		return fmt.Errorf("stage %q %s go_to %q does not reference an existing stage", stageID, name, goTo)
	}
	if rule.IssueLabels == nil {
		return fmt.Errorf("stage %q %s issue_labels is required", stageID, name)
	}
	if len(rule.IssueLabels.Labels) == 0 {
		return fmt.Errorf("stage %q %s issue_labels labels cannot be empty", stageID, name)
	}
	for i, label := range rule.IssueLabels.Labels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("stage %q %s issue_labels labels[%d] cannot be blank", stageID, name, i)
		}
	}
	return nil
}

func firstSkipCycle(edges map[string][]string) []string {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := map[string]int{}
	var stack []string
	var visit func(string) []string
	visit = func(stageID string) []string {
		state[stageID] = visiting
		stack = append(stack, stageID)
		for _, next := range edges[stageID] {
			switch state[next] {
			case visiting:
				for i, id := range stack {
					if id == next {
						return append(append([]string(nil), stack[i:]...), next)
					}
				}
			case unvisited:
				if cycle := visit(next); len(cycle) > 0 {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[stageID] = visited
		return nil
	}
	for stageID := range edges {
		if state[stageID] == unvisited {
			if cycle := visit(stageID); len(cycle) > 0 {
				return cycle
			}
		}
	}
	return nil
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

// HasPlanGate reports whether this pipeline opts into deterministic plan
// approval: at least one stage with plan_gate: true and a configured gate.
// Ordinary validation gates do not count — freeform hub plan approval remains
// the default for existing workflows.
func (p *Pipeline) HasPlanGate() bool {
	if p == nil {
		return false
	}
	for i := range p.Stages {
		if p.Stages[i].PlanGate && p.Stages[i].Gate != nil {
			return true
		}
	}
	return false
}

// StageByID returns the stage with the given ID, or nil if none exists.
func (p *Pipeline) StageByID(id string) *Stage {
	for i := range p.Stages {
		if p.Stages[i].ID == id {
			return &p.Stages[i]
		}
	}
	return nil
}

// StageTimeout is a resolved stage_timeout trigger.
type StageTimeout struct {
	Stage    *Stage
	After    time.Duration
	TargetID string
	Target   *Stage
}

// StagesWithTimeouts returns every stage with a valid stage_timeout trigger.
func (p *Pipeline) StagesWithTimeouts() []StageTimeout {
	if p == nil {
		return nil
	}
	var timeouts []StageTimeout
	for i := range p.Stages {
		for _, trigger := range p.Stages[i].Triggers {
			if trigger.StageTimeout == nil {
				continue
			}
			after, err := time.ParseDuration(strings.TrimSpace(trigger.StageTimeout.After))
			target := p.StageByID(strings.TrimSpace(trigger.StageTimeout.GoTo))
			if err != nil || after <= 0 || target == nil {
				continue
			}
			timeouts = append(timeouts, StageTimeout{Stage: &p.Stages[i], After: after, TargetID: target.ID, Target: target})
		}
	}
	return timeouts
}

// StageForMessageContains returns the first stage whose message_contains
// trigger token opens a line of the given message. Returns nil if none match.
// The matching rule is MessageSignals; read it before changing this.
func (p *Pipeline) StageForMessageContains(message string) *Stage {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.MessageContains != "" && MessageSignals(message, t.MessageContains) {
				return &p.Stages[i]
			}
		}
	}
	return nil
}

// MessageContainsTokens returns the distinct message_contains tokens declared by
// this pipeline, in stage order. It exists so the hub can tell which tokens are
// meaningful to a given run without re-parsing triggers, which is what the
// unanchored-signal nudge needs.
// StageIDForMessageContainsToken returns the id of the first stage triggered by
// this token, or "" if none declares it. The caller needs it to tell a token
// whose stage is still ahead from one whose stage is already behind.
func (p *Pipeline) StageIDForMessageContainsToken(token string) string {
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.MessageContains != "" && strings.EqualFold(t.MessageContains, token) {
				return p.Stages[i].ID
			}
		}
	}
	return ""
}

func (p *Pipeline) MessageContainsTokens() []string {
	var tokens []string
	seen := map[string]bool{}
	for i := range p.Stages {
		for _, t := range p.Stages[i].Triggers {
			if t.MessageContains == "" {
				continue
			}
			key := strings.ToUpper(t.MessageContains)
			if seen[key] {
				continue
			}
			seen[key] = true
			tokens = append(tokens, t.MessageContains)
		}
	}
	return tokens
}

// MessageSignals reports whether message emits token as a signal: the token must
// ANCHOR at the start of some line of the message, ignoring leading whitespace
// and markdown decoration. It is the single definition of "the agent sent this
// signal", and every caller in the hub must use it — the pipeline trigger path,
// the legacy [DONE] and [TERMINATE] gates in server.go, and handleClawDoneSignal
// itself. One message getting two different verdicts on two code paths is how
// NEXT-707 turned a partial fix into a worse bug than the original.
//
// A bare substring match burned us twice in two days. NEXT-724: a claw that had
// no ticket yet described the process it would follow — "5. Commit locally and
// signal `[IMPLEMENTATION_DONE]` per the lifecycle table" — and the hub jumped
// straight to review_loop; the run died 4h20 later at $6.64 with zero PRs.
// NEXT-707: an agent reported a legitimate blocker with "sending `[DONE]` now
// would be a false signal", and the hub opened a PR stage, stamped Agent
// Finished on Linear and pinged Slack for human review — the agent was declared
// done because it said it was NOT declaring itself done.
//
// Anchoring is unconditional, not a per-pipeline opt-in: the hub is shared by
// factories we do not own, and a flag defaulting either way is wrong. Defaulting
// off leaves every factory carrying the bug that cost these two runs; defaulting
// on makes the flag dead weight nobody would ever set.
//
// This tightens the contract rather than restating it. The injects in this repo
// were NOT uniform — .elasticclaw/workflows/github-issue.yaml only said "say
// [DONE]" — so anchoring is what makes them uniform, and they have been reworded
// to match. Injects we do not own (other repos' workflows) may still ask for the
// token loosely; MessageMentionsUnanchored plus the hub's nudge is the safety net
// for those, converting a silent freeze into a recoverable one.
func MessageSignals(message, token string) bool {
	if token == "" {
		return false
	}
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimLeft(line, signalDecoration)
		if len(line) >= len(token) && equalFold(line[:len(token)], token) {
			return true
		}
	}
	return false
}

// MessageMentionsUnanchored reports whether token appears somewhere in message
// but never opens a line — precisely the shape that used to transition the
// pipeline and now does not. The hub uses it to nudge the agent to resend, so
// that a mis-formatted signal fails loudly instead of hanging the run forever.
func MessageMentionsUnanchored(message, token string) bool {
	if token == "" {
		return false
	}
	return containsFold(message, token) && !MessageSignals(message, token)
}

// signalDecoration is what may sit between the start of a line and the signal
// token. A missed signal is worse than a spurious one — a spurious transition
// wastes a run, a missed one freezes the pipeline forever — so this has to
// tolerate how agents actually decorate the token. Observed in this hub:
// **[REVIEW_LOOP_PASSED]** (bold), `[DONE]` (backticked), and the bare token.
// Underscore joins asterisk because it is the same markdown emphasis marker, and
// '#' because a heading marker only ever exists at the start of a line.
//
// Blockquote '>' is deliberately excluded: quoting the inject back is exactly the
// NEXT-707 shape, and no agent has ever been seen signalling from inside a quote.
// List markers ('-', '1.') are excluded for the same reason — NEXT-724 was a
// numbered list item.
//
// '\r' is deliberately absent. It looks like it belongs next to '\t', but the
// split below is on '\n', so a CRLF message always leaves '\r' at the END of the
// preceding line and never at the start of one: it would pin nothing and protect
// nothing. Every character here is covered by exactly one case in
// TestStageForMessageContainsAnchoring; add a case with the character, or do not
// add the character.
const signalDecoration = " \t*_`#"

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

// containsFold reports whether s contains substr, case-insensitively. Only
// MessageMentionsUnanchored may use it: an unanchored mention is grounds for a
// nudge, never for a stage transition.
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
