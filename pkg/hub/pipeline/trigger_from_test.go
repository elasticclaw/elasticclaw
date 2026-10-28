package pipeline_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func TestTriggerFromMessageContains(t *testing.T) {
	for _, scope := range []string{"from: working", "from: [working, fix_build]", ""} {
		t.Run(scope, func(t *testing.T) {
			p, err := pipeline.Parse([]byte(fmt.Sprintf(`stages:
  - id: working
  - id: fix_build
  - id: other
  - id: build_check
    triggers:
      - message_contains: "[RETRY_BUILD]"
        %s
`, scope)))
			if err != nil {
				t.Fatal(err)
			}
			for _, stage := range []string{"working", "fix_build", "other", ""} {
				want := scope == "" || stage == "working" || (strings.Contains(scope, "[") && stage == "fix_build")
				if got := p.StageForMessageContains("**[retry_build]**", stage); (got != nil) != want {
					t.Errorf("stage %q: got %v, want match=%v", stage, got, want)
				}
			}
			if got := p.StageForMessageContains("[RETRY_BUILD]"); (got != nil) != (scope == "") {
				t.Errorf("without current stage: got %v", got)
			}
		})
	}
}

func TestTriggerFromAllKinds(t *testing.T) {
	for _, kind := range []string{
		`message_contains: "[DONE]"`, `pr_merged:`, `pr_closed:`,
		`pr_conditions: {ci: passing}`, `judge_verdict: pass`,
		`gate_result: {stage: working, verdict: pass}`,
		`output_matches: {output: build, path: status, any_of: [pass]}`,
	} {
		t.Run(kind, func(t *testing.T) {
			p, err := pipeline.Parse([]byte("stages:\n  - id: working\n  - id: other\n  - id: done\n    triggers:\n      - " + kind + "\n        from: working\n"))
			if err != nil {
				t.Fatal(err)
			}
			match := func(stage string) *pipeline.Stage {
				switch {
				case strings.HasPrefix(kind, "message_contains"):
					return p.StageForMessageContains("[DONE]", stage)
				case strings.HasPrefix(kind, "pr_merged"):
					return p.StageForPRMerged(stage)
				case strings.HasPrefix(kind, "pr_closed"):
					return p.StageForPRClosed(stage)
				case strings.HasPrefix(kind, "pr_conditions"):
					return p.StageForPRConditions(stage)
				case strings.HasPrefix(kind, "judge_verdict"):
					return p.StageForJudgeVerdict("pass", stage)
				case strings.HasPrefix(kind, "gate_result"):
					return p.StageForGateResult("working", "pass", stage)
				default:
					return p.StageForOutputMatches(map[string]map[string]interface{}{"build": {"status": "pass"}}, stage)
				}
			}
			if match("other") != nil {
				t.Fatal("trigger matched outside its source stage")
			}
			if got := match("working"); got == nil || got.ID != "done" {
				t.Fatalf("trigger did not match in its source stage: %v", got)
			}
		})
	}
}

func TestTriggerFromValidationAndNormalization(t *testing.T) {
	for _, scope := range []string{"working", "[working, fix_build]", "missing", "[working, missing]", "[]", "null", "true", "[working, 42]", `""`, "{stage: working}"} {
		t.Run(scope, func(t *testing.T) {
			data := []byte("name: scoped\nstages:\n  - id: working\n  - id: fix_build\n  - id: done\n    triggers:\n      - message_contains: '[DONE]'\n        from: " + scope + "\n")
			valid := scope == "working" || scope == "[working, fix_build]"
			p, err := pipeline.Parse(data)
			if (err == nil) != valid {
				t.Fatalf("parse error=%v, want valid=%v", err, valid)
			}
			if !valid && !strings.Contains(err.Error(), "from") {
				t.Fatalf("error lacks from context: %v", err)
			}
			var wf types.WorkflowConfig
			if err := yaml.Unmarshal(data, &wf); err != nil {
				t.Fatal(err)
			}
			if err := wf.Validate(); (err == nil) != valid {
				t.Fatalf("workflow validation error=%v, want valid=%v", err, valid)
			}
			if err := types.NormalizeWorkflowConfig(&wf); (err == nil) != valid {
				t.Fatalf("normalization error=%v, want valid=%v", err, valid)
			}
			if valid {
				roundTrip, err := pipeline.Parse([]byte(wf.PipelineYAML))
				if err != nil || !reflect.DeepEqual(p.Stages[2].Triggers, roundTrip.Stages[2].Triggers) {
					t.Fatalf("trigger changed during normalization: %v", err)
				}
			}
		})
	}
}

func TestRejectedMessageContainsTokens(t *testing.T) {
	p, err := pipeline.Parse([]byte(`stages:
  - id: working
  - id: other
  - id: done
    triggers:
      - message_contains: "[DONE]"
        from: other
      - message_contains: "[SHARED]"
        from: other
      - message_contains: "[shared]"
        from: working
      - message_contains: "[GLOBAL]"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.RejectedMessageContainsTokens("[DONE]\n[SHARED]\n[GLOBAL]", "working"); !reflect.DeepEqual(got, []string{"[DONE]"}) {
		t.Fatalf("rejected tokens = %v", got)
	}
	if got := p.RejectedMessageContainsTokens("I mentioned [DONE]", "working"); len(got) != 0 {
		t.Fatalf("unanchored token rejected: %v", got)
	}
	if got := p.MessageContainsTokens("working"); !reflect.DeepEqual(got, []string{"[shared]", "[GLOBAL]"}) {
		t.Fatalf("allowed tokens = %v", got)
	}
}
