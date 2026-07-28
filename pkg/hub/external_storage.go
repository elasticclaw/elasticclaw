package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

// hubDataDir returns the hub's data directory (where hub.yaml lives).
// It mirrors the logic in cmd/hub.go: /var/lib/elasticclaw for system installs,
// ~/.elasticclaw otherwise.
func hubDataDir() string {
	if _, err := os.Stat("/etc/elasticclaw/hub.yaml"); err == nil {
		return "/var/lib/elasticclaw"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".elasticclaw")
}

// hubConfigDir returns the directory containing hub.yaml.
func hubConfigDir() string {
	if env := os.Getenv("ELASTICCLAW_HUB_CONFIG"); env != "" {
		return filepath.Dir(env)
	}
	if _, err := os.Stat("/etc/elasticclaw/hub.yaml"); err == nil {
		return "/etc/elasticclaw"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".elasticclaw")
}

// factoriesDir returns the path to the external factories directory.
func factoriesDir() string {
	return filepath.Join(hubConfigDir(), "factories")
}

// templatesDir returns the path to the external templates directory.
func templatesDir() string {
	return filepath.Join(hubConfigDir(), "templates")
}

// workspacesDir returns the path to the external workspaces directory.
func workspacesDir() string {
	return filepath.Join(hubConfigDir(), "workspaces")
}

// EnsureExternalDirs creates the factories/ and templates/ directories
// alongside hub.yaml if they don't exist.
func EnsureExternalDirs() error {
	for _, dir := range []string{factoriesDir(), templatesDir(), workspacesDir()} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}

// ── Templates ────────────────────────────────────────────────────────────────

// loadExternalTemplate reads a template from the external templates directory.
func loadExternalTemplate(name string) (map[string]string, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(templatesDir(), name)
	return config.ReadTemplateFiles(dir)
}

// validateName rejects names that could cause path traversal.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("name contains path traversal")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name contains path separator")
	}
	return nil
}

// reservedWorkspaceNames are the slugs used by the settings UI catch-all route
// (web/app/settings/[[...parts]]/sections.ts). A workspace named with any of
// these would collide with a system section, so they are forbidden.
// Keep this list in sync with the frontend VALID_SECTIONS.
var reservedWorkspaceNames = map[string]bool{
	"runtimes":            true,
	"models":              true,
	"github":              true,
	"authentication":      true,
	"issue-trackers":      true,
	"workspaces":          true,
	"workflows":           true,
	"workspace-analytics": true,
	"secrets":             true,
	"ai-config":           true,
	"mcp-servers":         true,
	"analytics":           true,
	"doctor":              true,
	"troubleshoot":        true,
}

// validateWorkspaceName rejects names that are unsafe for filesystem use or
// reserved by the settings UI routes.
func validateWorkspaceName(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if reservedWorkspaceNames[name] {
		return fmt.Errorf("workspace name %q is reserved", name)
	}
	return nil
}

// saveExternalTemplate writes a template to the external templates directory.
// Any files present on disk but absent from the new files map are removed
// so that deletions are persisted across pushes.
func saveExternalTemplate(name string, files map[string]string) error {
	if err := validateName(name); err != nil {
		return err
	}
	dir := filepath.Join(templatesDir(), name)
	// Remove and recreate the directory so stale files from previous pushes
	// are not re-read by ReadTemplateFiles (which walks the memory/ subtree).
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove old template dir %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for fname, content := range files {
		// Nested files (memory/**, scripts/**) must survive a template push, so
		// only reject paths that would escape the template dir.
		safeName, err := cleanWorkspaceFilePath(fname)
		if err != nil {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(safeName))
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return fmt.Errorf("mkdir for template file %s: %w", safeName, err)
		}
		if err := os.WriteFile(path, []byte(content), 0640); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// deleteExternalTemplate removes a template from the external directory.
func deleteExternalTemplate(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	dir := filepath.Join(templatesDir(), name)
	return os.RemoveAll(dir)
}

// listExternalTemplates returns all template names from the external directory.
func listExternalTemplates() ([]string, error) {
	entries, err := os.ReadDir(templatesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// ── Factories ────────────────────────────────────────────────────────────────

// loadExternalFactories scans the factories directory and returns all
// FactoryConfigs with PipelineYAML loaded from disk.
func loadExternalFactories() ([]*types.FactoryConfig, error) {
	dir := factoriesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read factories dir: %w", err)
	}

	var factories []*types.FactoryConfig
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		f, err := loadExternalFactory(e.Name())
		if err != nil {
			// Log but don't fail — one bad factory shouldn't break the rest
			fmt.Fprintf(os.Stderr, "[hub] skip factory %q: %v\n", e.Name(), err)
			continue
		}
		factories = append(factories, f)
	}
	return factories, nil
}

// resolveFactories returns the merged view of in-memory and external-storage
// factories. External takes precedence. Used by polling and webhook handlers
// so they always see the latest factories regardless of whether they were
// loaded from hub.yaml or pushed via CLI/API.
func (s *Server) resolveFactories() []*types.FactoryConfig {
	external, err := loadExternalFactories()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hub] loadExternalFactories: %v\n", err)
	}

	s.mu.RLock()
	mem := s.hubCfg.Factories
	s.mu.RUnlock()

	merged := make(map[string]*types.FactoryConfig, len(mem)+len(external))
	for _, f := range mem {
		if f == nil {
			continue
		}
		merged[f.Name] = f
	}
	for _, f := range external {
		if f == nil {
			continue
		}
		merged[f.Name] = f
	}

	result := make([]*types.FactoryConfig, 0, len(merged))
	for _, f := range merged {
		result = append(result, f)
	}
	return result
}

// loadExternalFactory reads a single factory from disk.
func loadExternalFactory(name string) (*types.FactoryConfig, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(factoriesDir(), name)

	factoryPath := filepath.Join(dir, "factory.yaml")
	data, err := os.ReadFile(factoryPath)
	if err != nil {
		return nil, fmt.Errorf("read factory.yaml: %w", err)
	}

	var f types.FactoryConfig
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse factory.yaml: %w", err)
	}

	// Load pipeline.yaml alongside if present
	pipelinePath := filepath.Join(dir, "pipeline.yaml")
	if pdata, err := os.ReadFile(pipelinePath); err == nil {
		f.PipelineYAML = string(pdata)
	}

	return &f, nil
}

// saveExternalFactory writes a factory to the external directory.
func saveExternalFactory(f *types.FactoryConfig) error {
	if f == nil || f.Name == "" {
		return fmt.Errorf("factory name required")
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if err := validateName(f.Name); err != nil {
		return err
	}

	dir := filepath.Join(factoriesDir(), f.Name)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Write factory.yaml (without PipelineYAML — that goes in pipeline.yaml)
	factoryCopy := *f
	factoryCopy.PipelineYAML = ""
	fdata, err := yaml.Marshal(&factoryCopy)
	if err != nil {
		return fmt.Errorf("marshal factory.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "factory.yaml"), fdata, 0640); err != nil {
		return fmt.Errorf("write factory.yaml: %w", err)
	}

	// Write or remove pipeline.yaml based on whether PipelineYAML is set
	pipelinePath := filepath.Join(dir, "pipeline.yaml")
	if f.PipelineYAML != "" {
		if err := os.WriteFile(pipelinePath, []byte(f.PipelineYAML), 0640); err != nil {
			return fmt.Errorf("write pipeline.yaml: %w", err)
		}
	} else {
		_ = os.Remove(pipelinePath)
	}

	return nil
}

// deleteExternalFactory removes a factory directory.
func deleteExternalFactory(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	dir := filepath.Join(factoriesDir(), name)
	return os.RemoveAll(dir)
}

// ── Workspaces ───────────────────────────────────────────────────────────────

func loadExternalWorkspaces() ([]*types.WorkspaceConfig, error) {
	entries, err := os.ReadDir(workspacesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspaces dir: %w", err)
	}

	var workspaces []*types.WorkspaceConfig
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		workspace, err := loadExternalWorkspace(e.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "[hub] skip workspace %q: %v\n", e.Name(), err)
			continue
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, nil
}

func loadExternalWorkspace(name string) (*types.WorkspaceConfig, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(workspacesDir(), name)
	workspace, err := loadExternalWorkspaceConfig(name)
	if err != nil {
		return nil, err
	}
	if workspace.Name == "" {
		workspace.Name = name
	}
	if files, err := config.ReadTemplateFiles(dir); err == nil {
		workspace.Files = files
	}

	workflowDir := filepath.Join(dir, "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read workflows dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(workflowDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", e.Name(), err)
		}
		var workflow types.WorkflowConfig
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			return nil, fmt.Errorf("parse workflow %s: %w", e.Name(), err)
		}
		workflow.RawConfig = string(data)
		if workflow.Name == "" {
			workflow.Name = strings.TrimSuffix(e.Name(), ".yaml")
		}
		if err := types.NormalizeWorkflowConfig(&workflow); err != nil {
			return nil, fmt.Errorf("normalize workflow %s: %w", e.Name(), err)
		}
		workspace.Workflows = append(workspace.Workflows, &workflow)
	}
	return &workspace, nil
}

func loadExternalWorkspaceConfig(name string) (types.WorkspaceConfig, error) {
	dir := filepath.Join(workspacesDir(), name)
	configPath := filepath.Join(dir, "elasticclaw-config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		legacyPath := filepath.Join(dir, "workspace.yaml")
		data, err = os.ReadFile(legacyPath)
		if err != nil {
			return types.WorkspaceConfig{}, fmt.Errorf("read elasticclaw-config.yaml: %w", err)
		}
		configPath = legacyPath
	}
	var workspace types.WorkspaceConfig
	if err := yaml.Unmarshal(data, &workspace); err != nil {
		return types.WorkspaceConfig{}, fmt.Errorf("parse %s: %w", filepath.Base(configPath), err)
	}
	return workspace, nil
}

func loadExternalWorkflowsByIntegration(integration string) ([]*types.WorkspaceConfig, error) {
	workspaces, err := loadExternalWorkspaces()
	if err != nil {
		return nil, err
	}
	var matched []*types.WorkspaceConfig
	for _, workspace := range workspaces {
		if workspace == nil {
			continue
		}
		copyWorkspace := *workspace
		copyWorkspace.Workflows = nil
		for _, workflow := range workspace.Workflows {
			if workflow != nil && strings.EqualFold(workflow.Integration, integration) {
				copyWorkspace.Workflows = append(copyWorkspace.Workflows, workflow)
			}
		}
		if len(copyWorkspace.Workflows) > 0 {
			matched = append(matched, &copyWorkspace)
		}
	}
	return matched, nil
}

func filterWorkflowWorkspacesByName(workspaces []*types.WorkspaceConfig, workspaceName string) []*types.WorkspaceConfig {
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		return workspaces
	}
	filtered := make([]*types.WorkspaceConfig, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace != nil && strings.EqualFold(workspace.Name, workspaceName) {
			filtered = append(filtered, workspace)
		}
	}
	return filtered
}

func saveExternalWorkspace(workspace *types.WorkspaceConfig) error {
	if workspace == nil || workspace.Name == "" {
		return fmt.Errorf("workspace name required")
	}
	if err := validateWorkspaceName(workspace.Name); err != nil {
		return err
	}
	if err := workspace.Validate(); err != nil {
		return err
	}

	dir := filepath.Join(workspacesDir(), workspace.Name)
	workflowDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflowDir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", workflowDir, err)
	}
	if err := removeWorkspaceAuthoredFiles(dir); err != nil {
		return err
	}

	data := []byte(workspace.Files["elasticclaw-config.yaml"])
	if len(strings.TrimSpace(string(data))) == 0 {
		var err error
		data, err = marshalWorkspaceElasticClawConfig(workspace, "")
		if err != nil {
			return fmt.Errorf("marshal elasticclaw-config.yaml: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), data, 0640); err != nil {
		return fmt.Errorf("write elasticclaw-config.yaml: %w", err)
	}
	for name, content := range workspace.Files {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "workflows/") || name == "workspace.yaml" || name == "elasticclaw-config.yaml" {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return fmt.Errorf("mkdir for workspace file %s: %w", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0640); err != nil {
			return fmt.Errorf("write workspace file %s: %w", name, err)
		}
	}
	return nil
}

func removeWorkspaceAuthoredFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "workflows" || name == workspaceManagedDirName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale workspace file %s: %w", name, err)
		}
	}
	return nil
}

func saveExternalWorkflows(workspaceName string, workflows []*types.WorkflowConfig) error {
	if err := validateName(workspaceName); err != nil {
		return err
	}
	if _, err := loadExternalWorkspaceConfig(workspaceName); err != nil {
		return err
	}
	workflowDir := filepath.Join(workspacesDir(), workspaceName, "workflows")
	if err := os.MkdirAll(workflowDir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", workflowDir, err)
	}
	for _, workflow := range workflows {
		if workflow == nil {
			continue
		}
		if err := validateName(workflow.Name); err != nil {
			return fmt.Errorf("workflow %q: %w", workflow.Name, err)
		}
		targetPath := filepath.Join(workflowDir, strings.ToLower(workflow.Name)+".yaml")
		if err := removeCaseVariantWorkflowFiles(workflowDir, workflow.Name, targetPath); err != nil {
			return err
		}
		data := []byte(workflow.RawConfig)
		if len(strings.TrimSpace(string(data))) == 0 {
			var err error
			data, err = yaml.Marshal(workflow)
			if err != nil {
				return fmt.Errorf("marshal workflow %q: %w", workflow.Name, err)
			}
		}
		if err := os.WriteFile(targetPath, data, 0640); err != nil {
			return fmt.Errorf("write workflow %q: %w", workflow.Name, err)
		}
	}
	return nil
}

func removeCaseVariantWorkflowFiles(workflowDir, workflowName, targetPath string) error {
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		return fmt.Errorf("read workflows dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(workflowDir, entry.Name())
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		if strings.EqualFold(name, workflowName) && path != targetPath {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove stale workflow %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func marshalWorkspaceElasticClawConfig(workspace *types.WorkspaceConfig, existing string) ([]byte, error) {
	values := map[string]interface{}{}
	if strings.TrimSpace(existing) != "" {
		if err := yaml.Unmarshal([]byte(existing), &values); err != nil {
			return nil, err
		}
	}
	values["name"] = workspace.Name
	if workspace.SchemaVersion != "" {
		values["schema_version"] = workspace.SchemaVersion
	} else if _, ok := values["schema_version"]; !ok {
		values["schema_version"] = "v1"
	}
	values["repositories"] = workspace.Repositories
	if len(workspace.Env) > 0 {
		values["env"] = workspace.Env
	} else if _, ok := values["env"]; !ok {
		values["env"] = map[string]string{}
	}
	delete(values, "github")
	if len(workspace.Secrets) > 0 {
		values["secrets"] = workspace.Secrets
	} else {
		delete(values, "secrets")
	}
	if len(workspace.WebhookSecrets) > 0 {
		values["webhook_secrets"] = workspace.WebhookSecrets
	} else {
		delete(values, "webhook_secrets")
	}
	return marshalOrderedYAML(values, []string{
		"schema_version",
		"name",
		"provider",
		"nix",
		"docker",
		"repositories",
		"env",
	})
}

func marshalOrderedYAML(values map[string]interface{}, order []string) ([]byte, error) {
	seen := map[string]bool{}
	root := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range order {
		value, ok := values[key]
		if !ok {
			continue
		}
		if err := appendYAMLMapEntry(root, key, value); err != nil {
			return nil, err
		}
		seen[key] = true
	}
	var rest []string
	for key := range values {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		if err := appendYAMLMapEntry(root, key, values[key]); err != nil {
			return nil, err
		}
	}
	return yaml.Marshal(root)
}

func appendYAMLMapEntry(root *yaml.Node, key string, value interface{}) error {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return err
	}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &node)
	return nil
}

func deleteExternalWorkspace(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(workspacesDir(), name))
}

// ── Migration ────────────────────────────────────────────────────────────────

// migrationMarkerPath returns the path to the migration marker file.
func migrationMarkerPath() string {
	return filepath.Join(hubDataDir(), ".migrated_v2")
}

// HasMigratedV2 checks if the v2 migration has already run.
func HasMigratedV2() bool {
	_, err := os.Stat(migrationMarkerPath())
	return err == nil
}

// MarkMigratedV2 writes the migration marker.
func MarkMigratedV2() error {
	return os.WriteFile(migrationMarkerPath(), []byte(""), 0644)
}

// MigrateLegacyTemplates migrates templates from the SQLite hub_templates table
// to the external templates/ directory, then drops the legacy table.
func (s *Server) MigrateLegacyTemplates() error {
	rows, err := s.db.Query(`SELECT name, files FROM hub_templates`)
	if err != nil {
		return fmt.Errorf("query hub_templates: %w", err)
	}
	defer rows.Close()

	var migrated int
	var migrationErrs []string
	for rows.Next() {
		var name, filesJSON string
		if err := rows.Scan(&name, &filesJSON); err != nil {
			migrationErrs = append(migrationErrs, err.Error())
			continue
		}
		var files map[string]string
		if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
			migrationErrs = append(migrationErrs, fmt.Sprintf("%s: parse files JSON: %v", name, err))
			continue
		}
		if err := saveExternalTemplate(name, files); err != nil {
			migrationErrs = append(migrationErrs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		migrated++
		fmt.Printf("[hub] migrated template %q to external storage\n", name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate templates: %w", err)
	}
	if len(migrationErrs) > 0 {
		return fmt.Errorf("migrate templates failed for %d row(s): %s", len(migrationErrs), strings.Join(migrationErrs, "; "))
	}

	// Drop legacy table so future runs never see it
	if migrated > 0 {
		if _, err := s.db.Exec(`DROP TABLE IF EXISTS hub_templates`); err != nil {
			return fmt.Errorf("drop hub_templates: %w", err)
		}
		fmt.Println("[hub] dropped legacy hub_templates table")
	}
	return nil
}

// MigrateLegacyFactories migrates factories from hub.yaml to the external
// factories/ directory, then strips them from hub.yaml. Returns the list of
// migrated factory names.
func MigrateLegacyFactories(cfg *types.HubConfig) ([]string, error) {
	var migrated []string
	for _, f := range cfg.Factories {
		if f == nil {
			continue
		}
		if err := saveExternalFactory(f); err != nil {
			fmt.Fprintf(os.Stderr, "[hub] migrate factory %q: %v\n", f.Name, err)
			continue
		}
		migrated = append(migrated, f.Name)
	}

	// Strip factories from hub.yaml so they are never loaded from legacy location again.
	// This runs even if len(migrated)==0 (all factories already on disk) to prevent
	// split-brain where hub.yaml and external storage both claim ownership.
	cfg.Factories = nil
	if err := config.SaveHubConfig(cfg); err != nil {
		return migrated, fmt.Errorf("strip factories from hub.yaml: %w", err)
	}
	if len(migrated) > 0 {
		fmt.Println("[hub] stripped migrated factories from hub.yaml")
	} else {
		fmt.Println("[hub] no inline factories to migrate — cleared factories from hub.yaml")
	}
	return migrated, nil
}
