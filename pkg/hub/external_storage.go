package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// EnsureExternalDirs creates the factories/ and templates/ directories
// alongside hub.yaml if they don't exist.
func EnsureExternalDirs() error {
	for _, dir := range []string{factoriesDir(), templatesDir()} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}

// ── Templates ────────────────────────────────────────────────────────────────

// loadExternalTemplate reads a template from the external templates directory.
func loadExternalTemplate(name string) (map[string]string, error) {
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

// saveExternalTemplate writes a template to the external templates directory.
func saveExternalTemplate(name string, files map[string]string) error {
	if err := validateName(name); err != nil {
		return err
	}
	dir := filepath.Join(templatesDir(), name)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for fname, content := range files {
		if strings.Contains(fname, "..") || strings.ContainsAny(fname, `/\`) {
			continue // skip paths with directory traversal
		}
		path := filepath.Join(dir, fname)
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

// loadExternalFactory reads a single factory from disk.
func loadExternalFactory(name string) (*types.FactoryConfig, error) {
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

	// Write pipeline.yaml if present
	if f.PipelineYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "pipeline.yaml"), []byte(f.PipelineYAML), 0640); err != nil {
			return fmt.Errorf("write pipeline.yaml: %w", err)
		}
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

// ── Migration ────────────────────────────────────────────────────────────────

// migrationMarkerPath returns the path to the migration marker file.
func migrationMarkerPath() string {
	return filepath.Join(hubDataDir(), ".migrated_v2")
}

// hasMigratedV2 checks if the v2 migration has already run.
func hasMigratedV2() bool {
	_, err := os.Stat(migrationMarkerPath())
	return err == nil
}

// markMigratedV2 writes the migration marker.
func markMigratedV2() error {
	return os.WriteFile(migrationMarkerPath(), []byte(""), 0644)
}

// MigrateLegacyTemplates migrates templates from the SQLite hub_templates table
// to the external templates/ directory.
func (s *Server) MigrateLegacyTemplates() error {
	rows, err := s.db.Query(`SELECT name, files FROM hub_templates`)
	if err != nil {
		return fmt.Errorf("query hub_templates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, filesJSON string
		if err := rows.Scan(&name, &filesJSON); err != nil {
			continue
		}
		var files map[string]string
		if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
			continue
		}
		if err := saveExternalTemplate(name, files); err != nil {
			fmt.Fprintf(os.Stderr, "[hub] migrate template %q: %v\n", name, err)
			continue
		}
		fmt.Printf("[hub] migrated template %q to external storage\n", name)
	}
	return rows.Err()
}

// MigrateLegacyFactories migrates factories from hub.yaml to the external
// factories/ directory. Returns the list of migrated factory names.
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
	return migrated, nil
}
