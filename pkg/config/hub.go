package config

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

var httpGet = http.Get

// ─── Consolidated hub boot loader ─────────────────────────────────────────────
//
// The hub's configuration is owned by this package (Viper stays CLI-only).
// Settings are resolved with the documented precedence:
//
//	flag > env (ELASTICCLAW_*) > hub.yaml > default
//
// Restart-required settings: listen_addr (ELASTICCLAW_ADDR / --addr) and
// db_path (ELASTICCLAW_DB / --db). Hot-reloadable settings (applied on
// SIGHUP via ApplyHotReload): log_level, allowed_origins, branding,
// integrations.

// Environment variable names honored by the hub loader.
const (
	EnvHubAddr           = "ELASTICCLAW_ADDR"
	EnvHubDBPath         = "ELASTICCLAW_DB"
	EnvHubLogLevel       = "ELASTICCLAW_LOG_LEVEL"
	EnvHubAllowedOrigins = "ELASTICCLAW_ALLOWED_ORIGINS" // comma-separated
)

// Defaults applied when neither flag, env, nor hub.yaml set a value.
const (
	DefaultHubAddr     = ":8080"
	DefaultHubLogLevel = "info"
)

// HubBootFlags carries the CLI flag values that participate in precedence
// resolution. An empty string means "flag not set".
type HubBootFlags struct {
	Addr     string
	DBPath   string
	LogLevel string
}

// HubBootConfig is the fully resolved hub configuration at boot time.
type HubBootConfig struct {
	// Cfg is the parsed hub.yaml (never nil).
	Cfg *types.HubConfig
	// Addr is the resolved listen address (restart required to change).
	Addr string
	// DBPath is the resolved SQLite path. Empty means "use the caller's
	// platform default" (~/.elasticclaw/hub.db or /var/lib/elasticclaw/hub.db).
	DBPath string
	// LogLevel is the resolved, validated log level (debug|info|warn|error).
	LogLevel string
	// AllowedOrigins is the resolved CORS origin allowlist (empty = allow all).
	AllowedOrigins []string
}

// LoadHubBootConfig loads hub.yaml and resolves the process-level settings
// with the precedence flag > env > hub.yaml > default, then validates them.
// This is the single entry point the hub server uses at boot.
func LoadHubBootConfig(flags HubBootFlags) (*HubBootConfig, error) {
	cfg, err := LoadHubConfig()
	if err != nil {
		return nil, err
	}

	boot := &HubBootConfig{
		Cfg:            cfg,
		Addr:           ResolveSetting(flags.Addr, EnvHubAddr, cfg.ListenAddr, DefaultHubAddr),
		DBPath:         ResolveSetting(flags.DBPath, EnvHubDBPath, cfg.DBPath, ""),
		LogLevel:       ResolveSetting(flags.LogLevel, EnvHubLogLevel, cfg.LogLevel, DefaultHubLogLevel),
		AllowedOrigins: ResolveOrigins(cfg.AllowedOrigins),
	}
	if err := boot.validate(); err != nil {
		return nil, err
	}
	return boot, nil
}

// ResolveSetting applies the documented precedence for a single setting:
// flag > env > hub.yaml > default. Empty values mean "not set".
func ResolveSetting(flagValue, envVar, yamlValue, defaultValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	if yamlValue != "" {
		return yamlValue
	}
	return defaultValue
}

// ResolveOrigins applies env > hub.yaml precedence for allowed_origins
// (there is no CLI flag for it). The env value is comma-separated.
func ResolveOrigins(yamlOrigins []string) []string {
	if v := os.Getenv(EnvHubAllowedOrigins); v != "" {
		var origins []string
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
		return origins
	}
	return yamlOrigins
}

// validate performs boot-time validation of the resolved settings.
func (b *HubBootConfig) validate() error {
	if _, err := ParseLogLevel(b.LogLevel); err != nil {
		return err
	}
	if !strings.Contains(b.Addr, ":") {
		return fmt.Errorf("invalid listen address %q: expected host:port or :port", b.Addr)
	}
	return nil
}

// ParseLogLevel converts a config string into an slog.Level.
// Empty input defaults to info.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log_level %q: must be one of debug, info, warn, error", s)
	}
}

// ApplyHotReload merges a freshly loaded hub.yaml into the current in-memory
// config, applying only the SIGHUP-safe subset: log_level, allowed_origins,
// branding, and integrations. It returns the merged config (a shallow copy of
// current — the input is never mutated), the names of the safe fields that
// changed, and the names of restart-required fields whose on-disk value
// changed and was therefore rejected.
func ApplyHotReload(current, fresh *types.HubConfig) (merged *types.HubConfig, applied, rejected []string) {
	cp := *current
	merged = &cp

	if fresh.LogLevel != current.LogLevel {
		merged.LogLevel = fresh.LogLevel
		applied = append(applied, "log_level")
	}
	if !slicesEqual(fresh.AllowedOrigins, current.AllowedOrigins) {
		merged.AllowedOrigins = fresh.AllowedOrigins
		applied = append(applied, "allowed_origins")
	}
	if !yamlEqual(fresh.Branding, current.Branding) {
		merged.Branding = fresh.Branding
		applied = append(applied, "branding")
	}
	if !yamlEqual(fresh.Integrations, current.Integrations) {
		merged.Integrations = fresh.Integrations
		applied = append(applied, "integrations")
	}

	// Restart-required fields: keep the current value and report the rejection.
	if fresh.ListenAddr != current.ListenAddr {
		rejected = append(rejected, "listen_addr")
	}
	if fresh.DBPath != current.DBPath {
		rejected = append(rejected, "db_path")
	}
	return merged, applied, rejected
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// yamlEqual compares two config subtrees by their YAML encoding. Good enough
// for change detection on small structs; avoids hand-written DeepEqual quirks.
func yamlEqual(a, b interface{}) bool {
	ab, errA := yaml.Marshal(a)
	bb, errB := yaml.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

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
	if env := os.Getenv("ELASTICCLAW_HUB_CONFIG"); env != "" {
		return env, nil
	}
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
	return saveHubConfig(cfg, true)
}

// SaveHubConfigNoSecretMerge writes hub config without merging disk secrets.
// Used by explicit secret delete/update paths where missing keys are intentional.
func SaveHubConfigNoSecretMerge(cfg *types.HubConfig) error {
	return saveHubConfig(cfg, false)
}

func saveHubConfig(cfg *types.HubConfig, mergeDiskSecrets bool) error {
	path, err := ActiveHubConfigPath()
	if err != nil {
		return err
	}

	// Work on a local copy so saving never mutates the caller's in-memory config.
	cfgToWrite := *cfg
	if cfg.Secrets != nil {
		cfgToWrite.Secrets = make(map[string]string, len(cfg.Secrets))
		for k, v := range cfg.Secrets {
			cfgToWrite.Secrets[k] = v
		}
	}

	if mergeDiskSecrets {
		// Merge: preserve any secrets that exist on disk but not in memory
		// (handles manual hub.yaml edits that were never loaded via API)
		if existing, err := LoadHubConfig(); err == nil && existing != nil {
			if cfgToWrite.Secrets == nil {
				cfgToWrite.Secrets = make(map[string]string)
			}
			for k, v := range existing.Secrets {
				if _, already := cfgToWrite.Secrets[k]; !already {
					cfgToWrite.Secrets[k] = v
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(&cfgToWrite)
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
	for i, r := range cfg.Repositories {
		if r.Repo == "" {
			return nil, fmt.Errorf("elasticclaw-config.yaml: repositories[%d] is missing 'repo' field", i)
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
	for i, r := range cfg.Repositories {
		if r.Repo == "" {
			return nil, fmt.Errorf("elasticclaw-config.yaml: repositories[%d] is missing 'repo' field (e.g. repo: owner/repo)", i)
		}
	}
	return cfg, nil
}

// ReadTemplateFiles reads all known OpenClaw workspace files from a template directory.
func ReadTemplateFiles(templateDir string) (map[string]string, error) {
	known := []string{
		"AGENTS.md", "SOUL.md", "TOOLS.md", "IDENTITY.md",
		"USER.md", "MEMORY.md", "BOOTSTRAP.md", "HEARTBEAT.md",
		"elasticclaw-config.yaml",
		"flake.nix", "flake.lock", // Nix flake for custom environment
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

	// Include scripts/ directory if present — workspace scripts delivered to claws.
	scriptsDir := filepath.Join(templateDir, "scripts")
	if stat, err := os.Stat(scriptsDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to stat scripts directory: %w", err)
		}
	} else if stat.IsDir() {
		if err := filepath.WalkDir(scriptsDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == scriptsDir {
				return nil
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(entry.Name(), ".") {
				return nil
			}
			rel, err := filepath.Rel(templateDir, path)
			if err != nil {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			files[filepath.ToSlash(rel)] = string(data)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("failed to read scripts directory: %w", err)
		}
	}

	return files, nil
}
