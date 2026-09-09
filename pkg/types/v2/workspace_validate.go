package v2

import (
	"bytes"
	"fmt"
	pathpkg "path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var secretNameRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var resourceNameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// known workspace top-level keys for v2 (unknown keys are rejected).
var knownWorkspaceKeys = map[string]bool{
	"schema_version": true,
	"name":           true,
	"repositories":   true,
	"execution":      true,
	"credentials":    true,
	"source_control": true,
	"ci":             true,
	"issue_trackers": true,
	"review_systems": true,
	"knowledge":      true,
}

// ParseWorkspace unmarshals workspace v2 YAML. It does not validate.
func ParseWorkspace(data []byte) (*Workspace, error) {
	version, err := DetectSchemaVersion(data)
	if err != nil {
		return nil, err
	}
	if !IsV2(version) {
		return nil, fmt.Errorf("workspace schema_version %q is not v2 (want 2 or v2)", version)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse workspace yaml: %w", err)
	}
	for key := range raw {
		if !knownWorkspaceKeys[key] {
			return nil, fmt.Errorf("workspace: unknown field %q", key)
		}
	}

	var ws Workspace
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&ws); err != nil {
		return nil, fmt.Errorf("parse workspace: %w", err)
	}
	ws.Raw = raw
	return &ws, nil
}

// ValidateWorkspace structurally validates a parsed workspace v2 document and
// returns a resolved view with capability intersections and content revision.
func ValidateWorkspace(ws *Workspace) (*ResolvedWorkspace, error) {
	if ws == nil {
		return nil, fmt.Errorf("workspace is nil")
	}
	if !IsV2(SchemaVersionString(ws.SchemaVersion)) {
		return nil, fmt.Errorf("workspace schema_version %q is not v2", SchemaVersionString(ws.SchemaVersion))
	}
	if strings.TrimSpace(ws.Name) == "" {
		return nil, fmt.Errorf("workspace name is required")
	}

	// Credentials: secret name refs only.
	for name, cred := range ws.Credentials {
		if err := validateResourceName("credentials", name); err != nil {
			return nil, err
		}
		secret := strings.TrimSpace(cred.Secret)
		if secret == "" {
			return nil, fmt.Errorf("credentials.%s.secret is required", name)
		}
		if !secretNameRegex.MatchString(secret) {
			return nil, fmt.Errorf("credentials.%s.secret %q must be a secret name reference (not a secret value)", name, secret)
		}
		// Reject obvious embedded material (multiline / PEM / whitespace).
		if strings.Contains(secret, "\n") || strings.Contains(secret, "BEGIN ") {
			return nil, fmt.Errorf("credentials.%s.secret must be a secret name reference, not embedded material", name)
		}
	}

	// Source-control connections.
	if ws.SourceControl != nil {
		for name, conn := range ws.SourceControl.Connections {
			if err := validateConnection("source_control.connections", name, conn, ws); err != nil {
				return nil, err
			}
		}
	}

	// Repositories.
	for name, repo := range ws.Repositories {
		if err := validateResourceName("repositories", name); err != nil {
			return nil, err
		}
		if strings.TrimSpace(repo.Provider) == "" {
			return nil, fmt.Errorf("repositories.%s.provider is required", name)
		}
		if strings.TrimSpace(repo.Repository) == "" {
			return nil, fmt.Errorf("repositories.%s.repository is required", name)
		}
		if sc := strings.TrimSpace(repo.SourceControl); sc != "" {
			if !ws.HasSourceControlConnection(sc) {
				return nil, fmt.Errorf("repositories.%s.source_control %q: unknown source_control connection", name, sc)
			}
		}
	}

	// CI connections and pipelines.
	resolvedCI := map[string]map[ConnectionCapability]bool{}
	if ws.CI != nil {
		for name, conn := range ws.CI.Connections {
			if err := validateConnection("ci.connections", name, conn, ws); err != nil {
				return nil, err
			}
			if sc := strings.TrimSpace(conn.SourceControl); sc != "" && !ws.HasSourceControlConnection(sc) {
				return nil, fmt.Errorf("ci.connections.%s.source_control %q: unknown source_control connection", name, sc)
			}
			if err := validateCapabilityRestrictions("ci.connections."+name, conn.Provider, conn.CapabilityRestrictions); err != nil {
				return nil, err
			}
			resolvedCI[name] = ResolveCapabilities(conn.Provider, conn.CapabilityRestrictions)
		}
		for name, pipe := range ws.CI.Pipelines {
			if err := validateResourceName("ci.pipelines", name); err != nil {
				return nil, err
			}
			if strings.TrimSpace(pipe.Connection) == "" {
				return nil, fmt.Errorf("ci.pipelines.%s.connection is required", name)
			}
			if !ws.HasCIConnection(pipe.Connection) {
				return nil, fmt.Errorf("ci.pipelines.%s.connection %q: unknown ci connection", name, pipe.Connection)
			}
			if strings.TrimSpace(pipe.Repository) == "" {
				return nil, fmt.Errorf("ci.pipelines.%s.repository is required", name)
			}
			if !ws.HasRepository(pipe.Repository) {
				return nil, fmt.Errorf("ci.pipelines.%s.repository %q: unknown repository", name, pipe.Repository)
			}
		}
	}

	// Issue trackers.
	resolvedIT := map[string]map[ConnectionCapability]bool{}
	if ws.IssueTrackers != nil {
		for name, conn := range ws.IssueTrackers.Connections {
			if err := validateConnection("issue_trackers.connections", name, conn, ws); err != nil {
				return nil, err
			}
			if err := validateCapabilityRestrictions("issue_trackers.connections."+name, conn.Provider, conn.CapabilityRestrictions); err != nil {
				return nil, err
			}
			resolvedIT[name] = ResolveCapabilities(conn.Provider, conn.CapabilityRestrictions)
		}
	}

	// Review systems.
	resolvedRS := map[string]map[ConnectionCapability]bool{}
	if ws.ReviewSystems != nil {
		for name, conn := range ws.ReviewSystems.Connections {
			if err := validateConnection("review_systems.connections", name, conn, ws); err != nil {
				return nil, err
			}
			if sc := strings.TrimSpace(conn.SourceControl); sc != "" && !ws.HasSourceControlConnection(sc) {
				return nil, fmt.Errorf("review_systems.connections.%s.source_control %q: unknown source_control connection", name, sc)
			}
			if err := validateCapabilityRestrictions("review_systems.connections."+name, conn.Provider, conn.CapabilityRestrictions); err != nil {
				return nil, err
			}
			resolvedRS[name] = ResolveCapabilities(conn.Provider, conn.CapabilityRestrictions)
		}
	}

	// Knowledge connections and sources. Sources are workspace-owned so a
	// workflow can never expand repository or credential authority.
	if ws.Knowledge != nil {
		for name, conn := range ws.Knowledge.Connections {
			if err := validateConnection("knowledge.connections", name, conn, ws); err != nil {
				return nil, err
			}
		}
		for name, source := range ws.Knowledge.Sources {
			if err := validateKnowledgeSource(name, source, ws); err != nil {
				return nil, err
			}
		}
	}

	resolvedSC := map[string]map[ConnectionCapability]bool{}
	if ws.SourceControl != nil {
		for name, conn := range ws.SourceControl.Connections {
			if err := validateCapabilityRestrictions("source_control.connections."+name, conn.Provider, conn.CapabilityRestrictions); err != nil {
				return nil, err
			}
			resolvedSC[name] = ResolveCapabilities(conn.Provider, conn.CapabilityRestrictions)
		}
	}

	// Execution block capabilities.
	resolvedExec := map[ConnectionCapability]bool{}
	if ws.Execution != nil {
		provider := strings.TrimSpace(ws.Execution.Provider)
		if provider == "" {
			return nil, fmt.Errorf("execution.provider is required")
		}
		if err := validateCapabilityRestrictions("execution", provider, ws.Execution.CapabilityRestrictions); err != nil {
			return nil, err
		}
		resolvedExec = ResolveCapabilities(provider, ws.Execution.CapabilityRestrictions)
	}

	rev, err := RevisionOf(ws)
	if err != nil {
		return nil, err
	}
	return &ResolvedWorkspace{
		Workspace:             ws,
		Revision:              rev,
		ResolvedCICaps:        resolvedCI,
		ResolvedSourceControl: resolvedSC,
		ResolvedIssueTrackers: resolvedIT,
		ResolvedReviewSystems: resolvedRS,
		ResolvedExecCaps:      resolvedExec,
	}, nil
}

func validateKnowledgeSource(name string, source KnowledgeSource, ws *Workspace) error {
	if err := validateResourceName("knowledge.sources", name); err != nil {
		return err
	}
	path := "knowledge.sources." + name
	kind := strings.TrimSpace(source.Type)
	switch kind {
	case KnowledgeTypeWorkspaceFiles, KnowledgeTypeRepositoryFiles, KnowledgeTypeRetrieval:
	case "":
		return fmt.Errorf("%s.type is required", path)
	default:
		return fmt.Errorf("%s.type %q is unsupported", path, kind)
	}
	scope := strings.TrimSpace(source.Scope)
	if scope != KnowledgeScopeOrganization && scope != KnowledgeScopeRepository {
		return fmt.Errorf("%s.scope %q must be %q or %q", path, scope, KnowledgeScopeOrganization, KnowledgeScopeRepository)
	}

	if kind == KnowledgeTypeRetrieval {
		if strings.TrimSpace(source.Connection) == "" {
			return fmt.Errorf("%s.connection is required for retrieval", path)
		}
		if !ws.HasKnowledgeConnection(source.Connection) {
			return fmt.Errorf("%s.connection %q: unknown knowledge connection", path, source.Connection)
		}
	} else if strings.TrimSpace(source.Connection) != "" {
		return fmt.Errorf("%s.connection is only valid for retrieval", path)
	}

	if kind == KnowledgeTypeWorkspaceFiles || kind == KnowledgeTypeRepositoryFiles {
		if len(source.Paths) == 0 {
			return fmt.Errorf("%s.paths is required for %s", path, kind)
		}
		for i, p := range source.Paths {
			p = strings.TrimSpace(p)
			if p == "" || strings.HasPrefix(p, "/") || pathpkg.Clean(p) != p || hasParentPathSegment(p) {
				return fmt.Errorf("%s.paths[%d] %q must be a non-empty relative path without '..'", path, i, p)
			}
		}
	}
	if kind != KnowledgeTypeRepositoryFiles && len(source.Repositories) > 0 {
		return fmt.Errorf("%s.repositories is only valid for repository_files", path)
	}
	for i, repo := range source.Repositories {
		if !ws.HasRepository(repo) {
			return fmt.Errorf("%s.repositories[%d] %q: unknown repository", path, i, repo)
		}
	}
	return nil
}

func hasParentPathSegment(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// ParseAndValidateWorkspace is the shipped entry point for workspace v2 YAML.
func ParseAndValidateWorkspace(data []byte) (*ResolvedWorkspace, error) {
	ws, err := ParseWorkspace(data)
	if err != nil {
		return nil, err
	}
	return ValidateWorkspace(ws)
}

func validateResourceName(section, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s: empty resource name", section)
	}
	if !resourceNameRegex.MatchString(name) {
		return fmt.Errorf("%s.%s: invalid resource name", section, name)
	}
	return nil
}

func validateConnection(path, name string, conn Connection, ws *Workspace) error {
	if err := validateResourceName(path, name); err != nil {
		return err
	}
	if strings.TrimSpace(conn.Provider) == "" {
		return fmt.Errorf("%s.%s.provider is required", path, name)
	}
	if cred := strings.TrimSpace(conn.Credentials); cred != "" {
		if !ws.HasCredential(cred) {
			return fmt.Errorf("%s.%s.credentials %q: unknown credential (missing credentials.%s)", path, name, cred, cred)
		}
	}
	return nil
}

func validateCapabilityRestrictions(path, provider string, restrictions map[string]bool) error {
	if len(restrictions) == 0 {
		return nil
	}
	providerCaps := ProviderCapabilities(provider)
	for capName, enabled := range restrictions {
		if strings.TrimSpace(capName) == "" {
			return fmt.Errorf("%s.capability_restrictions: empty capability name", path)
		}
		// Workspace may only narrow: setting true when provider lacks the cap invents a grant.
		if enabled {
			if !providerCaps[ConnectionCapability(capName)] {
				return fmt.Errorf("%s.capability_restrictions.%s: cannot grant capability unsupported by provider %q", path, capName, provider)
			}
		}
		// false is always a legal narrow (even for unknown names we still reject unknown?).
		// Unknown capability names that are set false are harmless narrows; reject unknown names
		// that are not in the global catalog to catch typos.
		if !isKnownCapability(capName) {
			return fmt.Errorf("%s.capability_restrictions.%s: unknown capability", path, capName)
		}
	}
	return nil
}

func isKnownCapability(name string) bool {
	for _, c := range AllConnectionCapabilities {
		if string(c) == name {
			return true
		}
	}
	return false
}
