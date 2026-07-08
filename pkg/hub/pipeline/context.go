package pipeline

import "github.com/elasticclaw/elasticclaw/pkg/types"

// Context identifies the factory or workflow a pipeline run belongs to. It
// moved here from pkg/hub (where it was the unexported pipelineContext) during
// the phase-2 hub reorganization so that both pkg/hub and the extracted
// subpackages (pkg/hub/integrations) can share it without an import cycle.
type Context struct {
	Factory              *types.FactoryConfig
	Workspace            *types.WorkspaceConfig
	Workflow             *types.WorkflowConfig
	IssueID              string
	IssueLabels          []string
	IssueLabelsAvailable bool
}

// Name returns a human-readable identifier for logging.
func (ctx Context) Name() string {
	if ctx.Workflow != nil && ctx.Workspace != nil {
		return "workflow:" + ctx.Workspace.Name + "/" + ctx.Workflow.Name
	}
	if ctx.Factory != nil {
		return "factory:" + ctx.Factory.Name
	}
	return "pipeline"
}

// Integration returns the issue-tracker integration the pipeline is bound to.
func (ctx Context) Integration() string {
	if ctx.Workflow != nil {
		return ctx.Workflow.Integration
	}
	if ctx.Factory != nil {
		return ctx.Factory.Integration
	}
	return ""
}

// TrackerName returns the workspace/tracker name the pipeline is bound to.
func (ctx Context) TrackerName() string {
	if ctx.Workflow != nil {
		return ctx.Workflow.Workspace
	}
	if ctx.Factory != nil {
		return ctx.Factory.Workspace
	}
	return ""
}

// PipelineYAML returns the raw pipeline definition for the context.
func (ctx Context) PipelineYAML() string {
	if ctx.Workflow != nil {
		return ctx.Workflow.PipelineYAML
	}
	if ctx.Factory != nil {
		return ctx.Factory.PipelineYAML
	}
	return ""
}
