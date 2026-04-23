package config

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

var httpGet = http.Get

// LoadHubConfig loads the hub configuration from (in order):
//  1. $ELASTICCLAW_HUB_CONFIG env var path
//  2. /etc/elasticclaw/hub.yaml
//  3. ~/.elasticclaw/hub.yaml
func LoadHubConfig() (*types.HubConfig, error) {
	candidates := hubConfigPaths()
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("failed to read hub config %s: %w", path, err)
		}
		cfg := &types.HubConfig{}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse hub config %s: %w", path, err)
		}
		return cfg, nil
	}
	return &types.HubConfig{}, nil
}

// SaveHubConfig writes hub config to ~/.elasticclaw/hub.yaml.
// ActiveHubConfigPath returns the path of the hub.yaml that was loaded.
// Used to save config changes back to the right file.
// Returns an error if no path can be determined.
func ActiveHubConfigPath() (string, error) {
	for _, p := range hubConfigPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	// Default: ~/.elasticclaw/hub.yaml
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine hub config path: %w", err)
	}
	return filepath.Join(home, ".elasticclaw", "hub.yaml"), nil
}

func SaveHubConfig(cfg *types.HubConfig) error {
	path, err := ActiveHubConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// Debug: log the factories section of what's being written
	for _, f := range cfg.Factories {
		log.Printf("[SaveHubConfig] factory %q repos=%v trigger=%+v", f.Name, f.Repos, f.Trigger)
	}
	log.Printf("[SaveHubConfig] yaml snippet (first 800 bytes):\n%s", func() string {
		if len(data) > 800 { return string(data[:800]) }
		return string(data)
	}())
	return os.WriteFile(path, data, 0600)
}

func hubConfigPaths() []string {
	var paths []string
	if env := os.Getenv("ELASTICCLAW_HUB_CONFIG"); env != "" {
		paths = append(paths, env)
	}
	paths = append(paths, "/etc/elasticclaw/hub.yaml")
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".elasticclaw", "hub.yaml"))
	}
	return paths
}

// TemplateRegistryURL is the GitHub repo used as the default template registry.
const TemplateRegistryURL = "https://github.com/elasticclaw/elasticclaw-templates"

// TemplateRegistryRawBase is the raw content base URL for fetching templates.
const TemplateRegistryRawBase = "https://raw.githubusercontent.com/elasticclaw/elasticclaw-templates/main/templates"

// ResolveTemplate finds a template by name, checking:
//  1. ./.elasticclaw/templates/<name>/ (repo-local)
//  2. ~/.elasticclaw/templates/<name>/ (global / cached)
//  3. Download from the public registry into ~/.elasticclaw/templates/<name>/
func ResolveTemplate(name string) (string, error) {
	// Repo-local first
	cwd, err := os.Getwd()
	if err == nil {
		local := filepath.Join(cwd, ".elasticclaw", "templates", name)
		if stat, err := os.Stat(local); err == nil && stat.IsDir() {
			return local, nil
		}
	}
	// Global / cached
	if home, err := os.UserHomeDir(); err == nil {
		global := filepath.Join(home, ".elasticclaw", "templates", name)
		if stat, err := os.Stat(global); err == nil && stat.IsDir() {
			return global, nil
		}
		// Not found locally — try downloading from registry
		if dlErr := downloadTemplate(name, global); dlErr == nil {
			return global, nil
		}
	}
	return "", fmt.Errorf("template %q not found locally or in registry (%s)", name, TemplateRegistryURL)
}

// downloadTemplate fetches a template from the public registry into dest.
func downloadTemplate(name, dest string) error {
	// Template files to fetch
	files := []string{"elasticclaw-config.yaml", "SOUL.md", "AGENTS.md", "TOOLS.md", "IDENTITY.md", "USER.md", "MEMORY.md", "BOOTSTRAP.md", "HEARTBEAT.md"}

	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}

	fetched := 0
	for _, f := range files {
		url := fmt.Sprintf("%s/%s/%s", TemplateRegistryRawBase, name, f)

		// Use net/http to fetch
		resp, err := httpGet(url)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue // file is optional
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dest, f), data, 0644); err != nil {
			os.RemoveAll(dest)
			return err
		}
		fetched++
	}

	if fetched == 0 {
		// Nothing fetched — template doesn't exist in registry
		os.RemoveAll(dest)
		return fmt.Errorf("template %q not found in registry", name)
	}
	return nil
}

// ParseTemplateConfig parses elasticclaw-config.yaml content from raw bytes.
// Used by the hub factory engine when the config is already loaded as a string
// (e.g. from the hub DB template store) rather than from the filesystem.
// Returns nil, nil when data is empty — caller treats missing config as "use defaults".
func ParseTemplateConfig(data []byte) (*types.TemplateConfig, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	cfg := &types.TemplateConfig{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("invalid elasticclaw-config.yaml: %w", err)
	}
	if cfg.GitHub != nil {
		for i, r := range cfg.GitHub.Repos {
			if r.Repo == "" {
				return nil, fmt.Errorf("elasticclaw-config.yaml: github.repos[%d] is missing 'repo' field", i)
			}
		}
	}
	return cfg, nil
}

// LoadTemplateConfig reads elasticclaw-config.yaml from a template directory.
func LoadTemplateConfig(templateDir string) (*types.TemplateConfig, error) {
	path := filepath.Join(templateDir, "elasticclaw-config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("template config not found at %s: %w", path, err)
	}
	cfg := &types.TemplateConfig{}
	// Use strict decoder so unknown fields produce an error rather than being silently ignored.
	// This catches typos like 'repository' instead of 'repo', 'instance-type' vs 'instance_type', etc.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		// Treat EOF (empty file) as valid empty config to allow provider validation to fire
		if err != io.EOF {
			return nil, fmt.Errorf("invalid elasticclaw-config.yaml: %w", err)
		}
	}
	if cfg.Provider == "" {
		return nil, fmt.Errorf("elasticclaw-config.yaml must specify a provider (e.g. provider: replicated)")
	}
	// Validate github repos
	if cfg.GitHub != nil {
		for i, r := range cfg.GitHub.Repos {
			if r.Repo == "" {
				return nil, fmt.Errorf("elasticclaw-config.yaml: github.repos[%d] is missing 'repo' field (e.g. repo: owner/repo)", i)
			}
		}
	}
	return cfg, nil
}

// ReadTemplateFiles reads all known OpenClaw workspace files from a template directory.
func ReadTemplateFiles(templateDir string) (map[string]string, error) {
	known := []string{
		"AGENTS.md", "SOUL.md", "TOOLS.md", "IDENTITY.md",
		"USER.md", "MEMORY.md", "BOOTSTRAP.md", "HEARTBEAT.md",
	}
	files := make(map[string]string)
	for _, name := range known {
		data, err := os.ReadFile(filepath.Join(templateDir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("failed to read %s: %w", name, err)
		}
		files[name] = string(data)
	}
	// Include memory/ directory if present (clean: no sensitive data)
	memDir := filepath.Join(templateDir, "memory")
	if entries, err := os.ReadDir(memDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
			if err != nil {
				continue
			}
			files["memory/"+e.Name()] = string(data)
		}
	}
	return files, nil
}


