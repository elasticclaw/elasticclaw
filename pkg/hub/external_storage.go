package hub

import (
	"encoding/json"
	"errors"
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

// templatesDir returns the path to the external templates directory.
func templatesDir() string {
	return filepath.Join(hubConfigDir(), "templates")
}

// workspacesDir returns the path to the external workspaces directory.
func workspacesDir() string {
	return filepath.Join(hubConfigDir(), "workspaces")
}

// legacyFactoriesDir is the retired on-disk factories/ tree. It is no longer
// created or loaded; deleteExternalFactory may still remove leftover entries
// so operators can clean a pre-migration hub.
func legacyFactoriesDir() string {
	return filepath.Join(hubConfigDir(), "factories")
}

// EnsureExternalDirs creates the templates/ and workspaces/ directories
// alongside hub.yaml if they don't exist.
func EnsureExternalDirs() error {
	for _, dir := range []string{templatesDir(), workspacesDir()} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}

// errFactoriesRetired is returned by factory save/load paths that used to
// target ~/.elasticclaw/factories. Automations must use workspace workflows.
var errFactoriesRetired = fmt.Errorf("factories are retired; use workspace workflows (elasticclaw workflow push --workspace <name>) instead of the factories/ directory")

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
	// Validate every key before touching the filesystem. The save below wipes the
	// existing template directory, so a bad key cannot be skipped: dropping it
	// after the wipe would destroy the previously stored template and persist a
	// silently truncated one while still reporting success. Nested files
	// (memory/**, scripts/**) must survive a push, so cleanWorkspaceFilePath
	// keeps them and rejects only unusable keys: paths that escape the template
	// dir, and empty or otherwise invalid names.
	safeNames := make(map[string]string, len(files))
	fnames := make([]string, 0, len(files))
	for fname := range files {
		fnames = append(fnames, fname)
	}
	sort.Strings(fnames)
	for _, fname := range fnames {
		safeName, err := cleanWorkspaceFilePath(fname)
		if err != nil {
			return fmt.Errorf("invalid template file path %q for %s: %w", fname, name, err)
		}
		safeNames[fname] = safeName
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
		safeName := safeNames[fname]
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

// ── Factories (retired on-disk storage) ──────────────────────────────────────
//
// Automations live under workspaces/*/workflows/. The factories/ directory is
// no longer loaded or written. resolveFactories only returns in-memory
// hubCfg.Factories (used by unit tests); production hubs keep that empty.

// loadExternalFactories used to scan factories/. It always returns an empty
// list so leftover on-disk factories cannot dual-fire with workflows.
func loadExternalFactories() ([]*types.FactoryConfig, error) {
	return nil, nil
}

// resolveFactories returns in-memory factories only (tests). Production hubs
// do not load factories from disk or hub.yaml after migration cleanup.
func (s *Server) resolveFactories() []*types.FactoryConfig {
	s.mu.RLock()
	mem := s.hubCfg.Factories
	s.mu.RUnlock()

	result := make([]*types.FactoryConfig, 0, len(mem))
	for _, f := range mem {
		if f == nil {
			continue
		}
		result = append(result, f)
	}
	return result
}

// loadExternalFactory always fails: on-disk factories are retired.
func loadExternalFactory(name string) (*types.FactoryConfig, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	return nil, errFactoriesRetired
}

// saveExternalFactory rejects writes: use workspace workflows instead.
func saveExternalFactory(f *types.FactoryConfig) error {
	if f == nil || f.Name == "" {
		return fmt.Errorf("factory name required")
	}
	if err := f.Validate(); err != nil {
		return err
	}
	return errFactoriesRetired
}

// deleteExternalFactory removes a leftover factories/<name> directory if
// present (best-effort cleanup). Missing dirs are not an error.
func deleteExternalFactory(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	dir := filepath.Join(legacyFactoriesDir(), name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return nil
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

// errWorkspaceNotFound is returned when a workspace name does not exist on the hub.
// Callers should map this to HTTP 404 rather than 500.
type errWorkspaceNotFound struct {
	Name string
}

func (e *errWorkspaceNotFound) Error() string {
	return fmt.Sprintf(
		"workspace %q not found on the hub; push it first with `elasticclaw workspace push --path <dir> %s`, then retry with `--workspace %s`",
		e.Name, e.Name, e.Name,
	)
}

func isWorkspaceNotFound(err error) bool {
	var target *errWorkspaceNotFound
	return errors.As(err, &target)
}

func loadExternalWorkspaceConfig(name string) (types.WorkspaceConfig, error) {
	dir := filepath.Join(workspacesDir(), name)
	if st, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return types.WorkspaceConfig{}, &errWorkspaceNotFound{Name: name}
		}
		return types.WorkspaceConfig{}, fmt.Errorf("stat workspace %q: %w", name, err)
	} else if !st.IsDir() {
		return types.WorkspaceConfig{}, fmt.Errorf("workspace %q path exists but is not a directory", name)
	}

	configPath := filepath.Join(dir, "elasticclaw-config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Only fall back to legacy workspace.yaml when the canonical file is
		// actually missing. Permissions / other I/O errors must surface as-is.
		if !os.IsNotExist(err) {
			return types.WorkspaceConfig{}, fmt.Errorf("read workspace %q config: %w", name, err)
		}
		legacyPath := filepath.Join(dir, "workspace.yaml")
		data, err = os.ReadFile(legacyPath)
		if err != nil {
			if os.IsNotExist(err) {
				return types.WorkspaceConfig{}, fmt.Errorf(
					"workspace %q exists but is missing elasticclaw-config.yaml (looked for elasticclaw-config.yaml and workspace.yaml)",
					name,
				)
			}
			return types.WorkspaceConfig{}, fmt.Errorf("read workspace %q config: %w", name, err)
		}
		configPath = legacyPath
	}
	var workspace types.WorkspaceConfig
	if err := yaml.Unmarshal(data, &workspace); err != nil {
		return types.WorkspaceConfig{}, fmt.Errorf("parse workspace %q %s: %w", name, filepath.Base(configPath), err)
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

	data := []byte(workspace.Files["elasticclaw-config.yaml"])
	if len(strings.TrimSpace(string(data))) == 0 {
		var err error
		data, err = marshalWorkspaceElasticClawConfig(workspace, "")
		if err != nil {
			return fmt.Errorf("marshal elasticclaw-config.yaml: %w", err)
		}
	}
	// Refuse invalid v2 workspace documents at the store boundary (RFC #544 inv. 28).
	// V1 documents continue through the existing WorkspaceConfig.Validate path above.
	// Validate before mutating the on-disk tree so a rejection cannot wipe existing files.
	if err := validateWorkspaceDocumentAtStore(data); err != nil {
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
	// Prefer raw YAML for store-time validation. V2 workspace documents use map-shaped
	// repositories/connections that cannot be unmarshaled into the v1 WorkspaceConfig type.
	workspaceYAML, err := readExternalWorkspaceYAML(workspaceName)
	if err != nil {
		// Fall back to the legacy typed load only to preserve the established
		// workspace-not-found error and HTTP 404 mapping. Other load failures keep
		// their existing context rather than being reported as v2 validation errors.
		if _, loadErr := loadExternalWorkspaceConfig(workspaceName); loadErr != nil {
			if isWorkspaceNotFound(loadErr) {
				return loadErr
			}
			return fmt.Errorf("load workspace %q: %w", workspaceName, loadErr)
		}
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
		// Refuse invalid v2 workflow documents (and invalid v2 workspace pairs).
		// V1 workflows keep the Normalize+Validate path at the API boundary.
		if err := validateWorkflowDocumentAtStore(data, workspaceYAML); err != nil {
			return fmt.Errorf("workflow %q: %w", workflow.Name, err)
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

// MigrateLegacyFactories strips factories from hub.yaml. On-disk factories/
// is no longer populated or loaded; automations must use workspace workflows.
// Returns names that were present in hub.yaml before clearing (for logging).
func MigrateLegacyFactories(cfg *types.HubConfig) ([]string, error) {
	var removed []string
	for _, f := range cfg.Factories {
		if f == nil || f.Name == "" {
			continue
		}
		removed = append(removed, f.Name)
	}

	cfg.Factories = nil
	if err := config.SaveHubConfig(cfg); err != nil {
		return removed, fmt.Errorf("strip factories from hub.yaml: %w", err)
	}
	if len(removed) > 0 {
		fmt.Printf("[hub] removed %d factory config(s) from hub.yaml (factories/ is retired; use workspace workflows): %s\n",
			len(removed), strings.Join(removed, ", "))
	}
	return removed, nil
}
