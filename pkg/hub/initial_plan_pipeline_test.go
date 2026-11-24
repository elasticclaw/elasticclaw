package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestClawNeedsInitialPlanPipelineOptOut(t *testing.T) {
	for _, tt := range []struct {
		name    string
		setting string
		want    bool
	}{
		{name: "default", want: true},
		{name: "enabled", setting: "initial_plan: true\n", want: true},
		{name: "disabled", setting: "initial_plan: false\n", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, db, _ := newClawRetryTestServer(t, "connected")
			s.hubCfg.Factories = []*types.FactoryConfig{{Name: "test", PipelineYAML: tt.setting + "stages:\n  - id: preflight\n    entry: true\n"}}
			if _, err := db.Exec(`UPDATE claws SET tags='["factory:test"]' WHERE id='retry-claw'`); err != nil {
				t.Fatal(err)
			}
			if got := s.clawNeedsInitialPlan("retry-claw"); got != tt.want {
				t.Fatalf("clawNeedsInitialPlan = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInitialPlanProceedRespectsPipeline(t *testing.T) {
	for _, tt := range []struct {
		name         string
		pipelineYAML string
		want         string
	}{
		{name: "freeform", want: `[hub] Initial plan received. Proceed with implementation. Keep sending visible progress updates before and after substantial work; tool calls and activity rows do not count as user communication.`},
		{name: "pipeline", pipelineYAML: "stages:\n  - id: preflight\n    entry: true\n", want: `[hub] Initial plan received. This is not a stage transition: continue with your current stage's instructions and end the stage with its signal. Keep sending visible progress updates; tool calls and activity rows do not count as user communication.`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, db, _ := newClawRetryTestServer(t, "connected")
			s.hubCfg.Factories = []*types.FactoryConfig{{Name: "test", PipelineYAML: tt.pipelineYAML}}
			if _, err := db.Exec(`UPDATE claws SET tags='["factory:test"]', pipeline_stage='preflight' WHERE id='retry-claw'`); err != nil {
				t.Fatal(err)
			}
			s.insertSystemMarker("retry-claw", "tenant", initialPlanRequiredMarker)
			plan := "I understand the issue is that the initial plan can skip pipeline stages. The likely code area is the hub server. My plan is to preserve the current stage instructions. I will verify the change with focused tests."
			s.handleInitialPlanResponse("retry-claw", "tenant", plan)
			var content string
			if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id='retry-claw' AND role='hub'`).Scan(&content); err != nil {
				t.Fatal(err)
			}
			if content != tt.want {
				t.Fatalf("proceed content = %q, want %q", content, tt.want)
			}
			var stage string
			if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id='retry-claw'`).Scan(&stage); err != nil {
				t.Fatal(err)
			}
			if stage != "preflight" {
				t.Fatalf("stage = %q, want preflight", stage)
			}
		})
	}
}
