package v2

// Workspace is the authored workspace v2 document (issue #544).
type Workspace struct {
	SchemaVersion interface{}            `yaml:"schema_version" json:"schema_version"`
	Name          string                 `yaml:"name" json:"name"`
	Repositories  map[string]Repository  `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Execution     *Execution             `yaml:"execution,omitempty" json:"execution,omitempty"`
	Credentials   map[string]Credential  `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	SourceControl *SourceControlBlock    `yaml:"source_control,omitempty" json:"source_control,omitempty"`
	CI            *CIBlock               `yaml:"ci,omitempty" json:"ci,omitempty"`
	IssueTrackers *ConnectionsOnlyBlock  `yaml:"issue_trackers,omitempty" json:"issue_trackers,omitempty"`
	ReviewSystems *ConnectionsOnlyBlock  `yaml:"review_systems,omitempty" json:"review_systems,omitempty"`
	Knowledge     *KnowledgeBlock        `yaml:"knowledge,omitempty" json:"knowledge,omitempty"`
	Raw           map[string]interface{} `yaml:"-" json:"-"`
}

// Repository is a named checkout target.
type Repository struct {
	Provider      string    `yaml:"provider" json:"provider"`
	Repository    string    `yaml:"repository" json:"repository"`
	SourceControl string    `yaml:"source_control,omitempty" json:"source_control,omitempty"`
	Checkout      *Checkout `yaml:"checkout,omitempty" json:"checkout,omitempty"`
}

// Checkout configures clone depth/ref.
type Checkout struct {
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Depth string `yaml:"depth,omitempty" json:"depth,omitempty"`
}

// Execution describes the agent execution environment.
type Execution struct {
	Provider string   `yaml:"provider,omitempty" json:"provider,omitempty"`
	Nix      bool     `yaml:"nix,omitempty" json:"nix,omitempty"`
	Docker   bool     `yaml:"docker,omitempty" json:"docker,omitempty"`
	Tools    []string `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// Credential is a named secret reference (name only; never a secret value).
type Credential struct {
	Secret string `yaml:"secret" json:"secret"`
}

// SourceControlBlock holds source-control connections.
type SourceControlBlock struct {
	Connections map[string]Connection `yaml:"connections,omitempty" json:"connections,omitempty"`
}

// CIBlock holds CI connections and repository-specific pipelines.
type CIBlock struct {
	Connections map[string]Connection `yaml:"connections,omitempty" json:"connections,omitempty"`
	Pipelines   map[string]Pipeline   `yaml:"pipelines,omitempty" json:"pipelines,omitempty"`
}

// ConnectionsOnlyBlock is used by issue_trackers and review_systems.
type ConnectionsOnlyBlock struct {
	Connections map[string]Connection `yaml:"connections,omitempty" json:"connections,omitempty"`
}

// KnowledgeBlock declares the organizational and repository context that the
// hub may assemble for a run. Workflows consume the resulting context bundle;
// they do not choose repositories or credentials themselves.
type KnowledgeBlock struct {
	Connections map[string]Connection      `yaml:"connections,omitempty" json:"connections,omitempty"`
	Sources     map[string]KnowledgeSource `yaml:"sources,omitempty" json:"sources,omitempty"`
}

// KnowledgeSource is one versionable input to run context assembly.
// Repository names, when present, refer to the workspace repository map. An
// empty repository list on repository_files means all dynamically relevant
// workspace repositories, not an unrestricted checkout.
type KnowledgeSource struct {
	Type         string                 `yaml:"type" json:"type"`
	Scope        string                 `yaml:"scope" json:"scope"`
	Required     bool                   `yaml:"required,omitempty" json:"required,omitempty"`
	Connection   string                 `yaml:"connection,omitempty" json:"connection,omitempty"`
	Repositories []string               `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Paths        []string               `yaml:"paths,omitempty" json:"paths,omitempty"`
	Query        string                 `yaml:"query,omitempty" json:"query,omitempty"`
	Parameters   map[string]interface{} `yaml:"parameters,omitempty" json:"parameters,omitempty"`
}

const (
	KnowledgeTypeWorkspaceFiles  = "workspace_files"
	KnowledgeTypeRepositoryFiles = "repository_files"
	KnowledgeTypeRetrieval       = "retrieval"

	KnowledgeScopeOrganization = "organization"
	KnowledgeScopeRepository   = "repository"
)

// Connection is a named provider endpoint + auth + optional capability narrows.
type Connection struct {
	Provider               string          `yaml:"provider" json:"provider"`
	BaseURL                string          `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Credentials            string          `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	SourceControl          string          `yaml:"source_control,omitempty" json:"source_control,omitempty"`
	CapabilityRestrictions map[string]bool `yaml:"capability_restrictions,omitempty" json:"capability_restrictions,omitempty"`
}

// Pipeline is a repository-specific CI workload bound to a connection.
type Pipeline struct {
	Connection string `yaml:"connection" json:"connection"`
	Repository string `yaml:"repository" json:"repository"`
	Workflow   string `yaml:"workflow,omitempty" json:"workflow,omitempty"`
	Project    string `yaml:"project,omitempty" json:"project,omitempty"`
	Pipeline   string `yaml:"pipeline,omitempty" json:"pipeline,omitempty"`
	Job        string `yaml:"job,omitempty" json:"job,omitempty"`
}

// ResolvedWorkspace is a validated workspace plus resolved connection capabilities.
type ResolvedWorkspace struct {
	Workspace             *Workspace
	Revision              ContentDigest
	ResolvedCICaps        map[string]map[ConnectionCapability]bool // connection name -> caps
	ResolvedSourceControl map[string]map[ConnectionCapability]bool
	ResolvedIssueTrackers map[string]map[ConnectionCapability]bool
	ResolvedReviewSystems map[string]map[ConnectionCapability]bool
}

// HasCIConnection reports whether a CI connection name exists.
func (w *Workspace) HasCIConnection(name string) bool {
	if w == nil || w.CI == nil || w.CI.Connections == nil {
		return false
	}
	_, ok := w.CI.Connections[name]
	return ok
}

// HasCIPipeline reports whether a CI pipeline name exists.
func (w *Workspace) HasCIPipeline(name string) bool {
	if w == nil || w.CI == nil || w.CI.Pipelines == nil {
		return false
	}
	_, ok := w.CI.Pipelines[name]
	return ok
}

// HasRepository reports whether a named repository exists.
func (w *Workspace) HasRepository(name string) bool {
	if w == nil || w.Repositories == nil {
		return false
	}
	_, ok := w.Repositories[name]
	return ok
}

// HasCredential reports whether a named credential exists.
func (w *Workspace) HasCredential(name string) bool {
	if w == nil || w.Credentials == nil {
		return false
	}
	_, ok := w.Credentials[name]
	return ok
}

// HasSourceControlConnection reports whether a source-control connection exists.
func (w *Workspace) HasSourceControlConnection(name string) bool {
	if w == nil || w.SourceControl == nil || w.SourceControl.Connections == nil {
		return false
	}
	_, ok := w.SourceControl.Connections[name]
	return ok
}

// HasIssueTrackerConnection reports whether an issue-tracker connection exists.
func (w *Workspace) HasIssueTrackerConnection(name string) bool {
	if w == nil || w.IssueTrackers == nil || w.IssueTrackers.Connections == nil {
		return false
	}
	_, ok := w.IssueTrackers.Connections[name]
	return ok
}

// HasReviewSystemConnection reports whether a review-system connection exists.
func (w *Workspace) HasReviewSystemConnection(name string) bool {
	if w == nil || w.ReviewSystems == nil || w.ReviewSystems.Connections == nil {
		return false
	}
	_, ok := w.ReviewSystems.Connections[name]
	return ok
}

// HasKnowledgeConnection reports whether a knowledge connection exists.
func (w *Workspace) HasKnowledgeConnection(name string) bool {
	if w == nil || w.Knowledge == nil || w.Knowledge.Connections == nil {
		return false
	}
	_, ok := w.Knowledge.Connections[name]
	return ok
}

// CIConnection returns a CI connection by name.
func (w *Workspace) CIConnection(name string) (Connection, bool) {
	if w == nil || w.CI == nil || w.CI.Connections == nil {
		return Connection{}, false
	}
	c, ok := w.CI.Connections[name]
	return c, ok
}

// CIPipeline returns a CI pipeline by name.
func (w *Workspace) CIPipeline(name string) (Pipeline, bool) {
	if w == nil || w.CI == nil || w.CI.Pipelines == nil {
		return Pipeline{}, false
	}
	p, ok := w.CI.Pipelines[name]
	return p, ok
}
