package config

import (
	"fmt"
	"io"
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
func SaveHubConfig(cfg *types.HubConfig) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".elasticclaw")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "hub.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
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
	files := []string{"elasticclaw-config.yaml", "SOUL.md", "AGENTS.md", "TOOLS.md", "IDENTITY.md", "USER.md", "MEMORY.md"}

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
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dest, f), data, 0644); err != nil {
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

// LoadTemplateConfig reads elasticclaw-config.yaml from a template directory.
func LoadTemplateConfig(templateDir string) (*types.TemplateConfig, error) {
	path := filepath.Join(templateDir, "elasticclaw-config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("template config not found at %s: %w", path, err)
	}
	cfg := &types.TemplateConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse template config: %w", err)
	}
	if cfg.Provider == "" {
		return nil, fmt.Errorf("template config must specify a provider")
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
