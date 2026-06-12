package types

import (
	"strings"
	"testing"
)

func TestWorkflowVolumeValidation(t *testing.T) {
	valid := &WorkflowConfig{
		Name: "volume-workflow",
		Volumes: []WorkflowVolume{{
			Name:   "fixtures",
			Source: "hub://volumes/test-fixtures",
			Mount:  "/mnt/elasticclaw/test-fixtures",
			Mode:   "ro",
		}, {
			Name:   "scratch",
			Source: "hub://volumes/scratch:latest",
			Mount:  "/mnt/elasticclaw/scratch",
			Mode:   "rw",
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid workflow volume config rejected: %v", err)
	}

	duplicateMount := &WorkflowConfig{
		Name: "volume-workflow",
		Volumes: []WorkflowVolume{{
			Name:   "fixtures",
			Source: "hub://volumes/test-fixtures",
			Mount:  "/mnt/elasticclaw/shared",
			Mode:   "ro",
		}, {
			Name:   "scratch",
			Source: "hub://volumes/scratch",
			Mount:  "/mnt/elasticclaw/shared",
			Mode:   "rw",
		}},
	}
	if err := duplicateMount.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate volume mount") {
		t.Fatalf("duplicate mount Validate() error = %v, want duplicate volume mount", err)
	}

	tests := []struct {
		name string
		vol  WorkflowVolume
		want string
	}{
		{name: "bad source", vol: WorkflowVolume{Name: "data", Source: "s3://bucket/data", Mount: "/mnt/elasticclaw/data", Mode: "ro"}, want: "source must use hub://volumes/<name>"},
		{name: "relative mount", vol: WorkflowVolume{Name: "data", Source: "hub://volumes/data", Mount: "data", Mode: "ro"}, want: "mount must be an absolute path"},
		{name: "workspace mount", vol: WorkflowVolume{Name: "data", Source: "hub://volumes/data", Mount: "/home/daytona/.openclaw/workspace/data", Mode: "ro"}, want: "outside the repository workspace"},
		{name: "bad mode", vol: WorkflowVolume{Name: "data", Source: "hub://volumes/data", Mount: "/mnt/elasticclaw/data", Mode: "rwx"}, want: "mode must be ro or rw"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := &WorkflowConfig{Name: "volume-workflow", Volumes: []WorkflowVolume{tt.vol}}
			err := workflow.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
