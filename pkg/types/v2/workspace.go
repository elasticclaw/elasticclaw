package v2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	Provider      string                `yaml:"provider" json:"provider"`
	Repository    string                `yaml:"repository" json:"repository"`
	SourceControl string                `yaml:"source_control,omitempty" json:"source_control,omitempty"`
	Checkout      *Checkout             `yaml:"checkout,omitempty" json:"checkout,omitempty"`
	Permissions   RepositoryPermissions `yaml:"permissions,omitempty" json:"permissions,omitzero"`
}

// RepositoryPermissions declares the GitHub App repository permissions a
// workspace repository needs. It accepts two authored forms:
//
//	permissions: write                        # scalar: "read" (default) or "write"
//	permissions:                              # granular: GitHub App permission names
//	  contents: write
//	  vulnerability_alerts: read              # Dependabot alerts
//	  security_events: read                   # code-scanning alerts
//
// The scalar form reproduces the historical default permission request:
// contents/pull_requests at the declared level, plus metadata/checks/statuses
// read, and the conditional workflows/issues scopes. The granular form keeps
// those defaults for every undeclared permission and only adds or overrides
// the named entries, so existing behavior is unchanged unless extras are
// declared.
type RepositoryPermissions struct {
	level    string
	granular map[string]string
}

// Level returns the base access level for the repository: "read" or "write".
// For the granular form it is derived from the declared contents level
// (defaulting to read) since contents drives clone/push access. Scalar values
// other than "write" normalize to "read", matching the historical projection
// behavior for existing configs.
func (p RepositoryPermissions) Level() string {
	if p.level == "write" {
		return "write"
	}
	if CanonicalGitHubPermissionLevel(p.granular["contents"]) == "write" {
		return "write"
	}
	return "read"
}

// Granular returns the canonicalized GitHub App permission map declared for
// this repository, or nil for the scalar form.
func (p RepositoryPermissions) Granular() map[string]string {
	if len(p.granular) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.granular))
	for name, level := range p.granular {
		out[CanonicalGitHubPermissionName(name)] = level
	}
	return out
}

// IsZero reports whether no permissions were declared.
func (p RepositoryPermissions) IsZero() bool {
	return p.level == "" && len(p.granular) == 0
}

// PermissionsFromLevel returns the scalar form ("read" or "write"). It is the
// programmatic construction path for callers that previously assigned a plain
// string to Repository.Permissions.
func PermissionsFromLevel(level string) RepositoryPermissions {
	return RepositoryPermissions{level: strings.ToLower(strings.TrimSpace(level))}
}

// PermissionsFromMap returns the granular form from a permission name -> level
// map. Keys are trimmed, lowercased, and canonicalized (aliases resolved);
// values are trimmed and lowercased. Use ValidateWorkspace to reject unknown
// names, invalid levels, and duplicates.
func PermissionsFromMap(granular map[string]string) RepositoryPermissions {
	if len(granular) == 0 {
		return RepositoryPermissions{}
	}
	out := make(map[string]string, len(granular))
	for name, level := range granular {
		out[CanonicalGitHubPermissionName(name)] = strings.ToLower(strings.TrimSpace(level))
	}
	return RepositoryPermissions{granular: out}
}

// UnmarshalYAML accepts the scalar ("read"/"write") and granular
// (permission name -> level) forms.
func (p *RepositoryPermissions) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!null" {
			return nil
		}
		p.level = strings.ToLower(strings.TrimSpace(value.Value))
		p.granular = nil
		return nil
	case yaml.MappingNode:
		granular := make(map[string]string, len(value.Content)/2)
		for i := 0; i+1 < len(value.Content); i += 2 {
			key, val := value.Content[i], value.Content[i+1]
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("permissions.%s: must be a permission level (read or write)", key.Value)
			}
			name := strings.ToLower(strings.TrimSpace(key.Value))
			if _, dup := granular[name]; dup {
				return fmt.Errorf("permissions.%s: duplicate declaration", name)
			}
			granular[name] = strings.ToLower(strings.TrimSpace(val.Value))
		}
		if len(granular) == 0 {
			return fmt.Errorf("permissions: granular form must declare at least one permission")
		}
		p.level = ""
		p.granular = granular
		return nil
	default:
		return fmt.Errorf("permissions: must be read, write, or a map of GitHub App permissions")
	}
}

// MarshalYAML round-trips the authored form.
func (p RepositoryPermissions) MarshalYAML() (interface{}, error) {
	if len(p.granular) > 0 {
		return p.granular, nil
	}
	if p.level == "" {
		return nil, nil
	}
	return p.level, nil
}

// UnmarshalJSON accepts the same forms from JSON payloads. The granular form
// is decoded in authored key order so duplicate keys (exact or differing only
// in case) are rejected instead of being silently collapsed by a plain map
// unmarshal.
func (p *RepositoryPermissions) UnmarshalJSON(data []byte) error {
	var scalar string
	if err := json.Unmarshal(data, &scalar); err == nil {
		p.level = strings.ToLower(strings.TrimSpace(scalar))
		p.granular = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	openTok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("permissions: must be read, write, or a map of GitHub App permissions")
	}
	if delim, ok := openTok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("permissions: must be read, write, or a map of GitHub App permissions")
	}
	granular := make(map[string]string)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("permissions: %v", err)
		}
		name, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("permissions: object keys must be strings")
		}
		levelTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("permissions.%s: %v", name, err)
		}
		level, ok := levelTok.(string)
		if !ok {
			return fmt.Errorf("permissions.%s: must be a permission level (read or write)", name)
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if _, dup := granular[key]; dup {
			return fmt.Errorf("permissions.%s: duplicate declaration", key)
		}
		granular[key] = strings.ToLower(strings.TrimSpace(level))
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return fmt.Errorf("permissions: %v", err)
	}
	if len(granular) == 0 {
		return fmt.Errorf("permissions: granular form must declare at least one permission")
	}
	p.level = ""
	p.granular = granular
	return nil
}

// MarshalJSON round-trips the authored form.
func (p RepositoryPermissions) MarshalJSON() ([]byte, error) {
	if len(p.granular) > 0 {
		return json.Marshal(p.granular)
	}
	if p.level == "" {
		return []byte("null"), nil
	}
	return json.Marshal(p.level)
}

// Checkout configures clone depth/ref.
type Checkout struct {
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Depth string `yaml:"depth,omitempty" json:"depth,omitempty"`
}

// Execution describes the agent execution environment.
type Execution struct {
	Provider               string          `yaml:"provider,omitempty" json:"provider,omitempty"`
	Nix                    bool            `yaml:"nix,omitempty" json:"nix,omitempty"`
	Docker                 bool            `yaml:"docker,omitempty" json:"docker,omitempty"`
	Tools                  []string        `yaml:"tools,omitempty" json:"tools,omitempty"`
	CapabilityRestrictions map[string]bool `yaml:"capability_restrictions,omitempty" json:"capability_restrictions,omitempty"`
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
	ResolvedExecCaps      map[ConnectionCapability]bool // execution-block capabilities
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
