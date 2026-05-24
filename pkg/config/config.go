package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

var validProfileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Paths returns the standard config paths
type Paths struct {
	ConfigDir    string // ~/.elasticclaw
	ConfigFile   string // ~/.elasticclaw/config.yaml
	ProfilesDir  string // ~/.elasticclaw/profiles
	StateDir     string // ~/.elasticclaw/state
	WorkDir      string // .elasticclaw (in current dir)
	TemplateDir  string // .elasticclaw/template
	LockFile     string // .elasticclaw/lock.yaml
	ManifestFile string // elasticclaw.yaml
}

// GetPaths returns the standard paths for the current environment
func GetPaths() (*Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	configDir := filepath.Join(home, ".elasticclaw")

	return &Paths{
		ConfigDir:    configDir,
		ConfigFile:   filepath.Join(configDir, "config.yaml"),
		ProfilesDir:  filepath.Join(configDir, "profiles"),
		StateDir:     filepath.Join(configDir, "state"),
		WorkDir:      filepath.Join(cwd, ".elasticclaw"),
		TemplateDir:  filepath.Join(cwd, ".elasticclaw", "template"),
		LockFile:     filepath.Join(cwd, ".elasticclaw", "lock.yaml"),
		ManifestFile: filepath.Join(cwd, "elasticclaw.yaml"),
	}, nil
}

// EnsureConfigDirs creates the config directory structure if it doesn't exist
func EnsureConfigDirs() error {
	paths, err := GetPaths()
	if err != nil {
		return err
	}

	dirs := []string{
		paths.ConfigDir,
		paths.ProfilesDir,
		paths.StateDir,
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// LoadGlobalConfig loads the global config file
func LoadGlobalConfig() (*types.GlobalConfig, error) {
	paths, err := GetPaths()
	if err != nil {
		return nil, err
	}

	cfg := &types.GlobalConfig{}

	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg, nil
}

// SaveGlobalConfig writes the global config file
func SaveGlobalConfig(cfg *types.GlobalConfig) error {
	paths, err := GetPaths()
	if err != nil {
		return err
	}

	if err := EnsureConfigDirs(); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(paths.ConfigFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// LoadProfile loads a profile by name
func LoadProfile(name string) (*types.Profile, error) {
	if !validProfileName.MatchString(name) {
		return nil, fmt.Errorf("invalid profile name %q", name)
	}
	paths, err := GetPaths()
	if err != nil {
		return nil, err
	}

	profileFile := filepath.Join(paths.ProfilesDir, name+".yaml")

	data, err := os.ReadFile(profileFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("profile %q not found", name)
		}
		return nil, fmt.Errorf("failed to read profile: %w", err)
	}

	profile := &types.Profile{}
	if err := yaml.Unmarshal(data, profile); err != nil {
		return nil, fmt.Errorf("failed to parse profile: %w", err)
	}
	profile.Name = name

	return profile, nil
}

// SaveProfile writes a profile
func SaveProfile(profile *types.Profile) error {
	if profile == nil || !validProfileName.MatchString(profile.Name) {
		return fmt.Errorf("invalid profile name %q", profileName(profile))
	}
	paths, err := GetPaths()
	if err != nil {
		return err
	}

	if err := EnsureConfigDirs(); err != nil {
		return err
	}

	profileFile := filepath.Join(paths.ProfilesDir, profile.Name+".yaml")

	data, err := yaml.Marshal(profile)
	if err != nil {
		return fmt.Errorf("failed to marshal profile: %w", err)
	}

	if err := os.WriteFile(profileFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write profile: %w", err)
	}

	return nil
}

// ListProfiles returns all available profiles
func ListProfiles() ([]*types.Profile, error) {
	paths, err := GetPaths()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(paths.ProfilesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read profiles directory: %w", err)
	}

	var profiles []*types.Profile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".yaml" {
			continue
		}
		name = name[:len(name)-5] // strip .yaml

		profile, err := LoadProfile(name)
		if err != nil {
			continue // skip invalid profiles
		}
		profiles = append(profiles, profile)
	}

	return profiles, nil
}

// DeleteProfile removes a profile
func DeleteProfile(name string) error {
	if !validProfileName.MatchString(name) {
		return fmt.Errorf("invalid profile name %q", name)
	}
	paths, err := GetPaths()
	if err != nil {
		return err
	}

	profileFile := filepath.Join(paths.ProfilesDir, name+".yaml")

	if err := os.Remove(profileFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("profile %q not found", name)
		}
		return fmt.Errorf("failed to delete profile: %w", err)
	}

	return nil
}

func profileName(profile *types.Profile) string {
	if profile == nil {
		return ""
	}
	return profile.Name
}

// LoadManifest loads the elasticclaw.yaml from the current directory
func LoadManifest() (*types.Manifest, error) {
	paths, err := GetPaths()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(paths.ManifestFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("elasticclaw.yaml not found - run 'elasticclaw init' first")
		}
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	manifest := &types.Manifest{}
	if err := yaml.Unmarshal(data, manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	return manifest, nil
}

// SaveManifest writes the elasticclaw.yaml
func SaveManifest(manifest *types.Manifest) error {
	paths, err := GetPaths()
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	if err := os.WriteFile(paths.ManifestFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	return nil
}

// LoadLockFile loads the .elasticclaw/lock.yaml
func LoadLockFile() (*types.LockFile, error) {
	paths, err := GetPaths()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(paths.LockFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read lock file: %w", err)
	}

	lock := &types.LockFile{}
	if err := yaml.Unmarshal(data, lock); err != nil {
		return nil, fmt.Errorf("failed to parse lock file: %w", err)
	}

	return lock, nil
}

// SaveLockFile writes the .elasticclaw/lock.yaml
func SaveLockFile(lock *types.LockFile) error {
	paths, err := GetPaths()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(paths.WorkDir, 0755); err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}

	data, err := yaml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("failed to marshal lock file: %w", err)
	}

	if err := os.WriteFile(paths.LockFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write lock file: %w", err)
	}

	return nil
}

// IsInitialized checks if the current directory has been initialized
func IsInitialized() bool {
	paths, err := GetPaths()
	if err != nil {
		return false
	}

	_, err = os.Stat(paths.WorkDir)
	return err == nil
}

// GetActiveProfile returns the currently active profile (legacy)
func GetActiveProfile() (*types.Profile, error) {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return nil, err
	}

	if cfg.ActiveProfile == "" {
		return &types.Profile{
			Name:     "default",
			Provider: "daytona",
			State:    "local",
		}, nil
	}

	return LoadProfile(cfg.ActiveProfile)
}

// ── Hub profile management ────────────────────────────────────────────────────

// ResolveHub returns the hub connection for the given profile name.
// Resolution order: profileFlag > ELASTICCLAW_PROFILE env > active_profile > "default".
// Migrates legacy flat cfg.Hub to profiles["default"] transparently.
func ResolveHub(profileFlag string) (*types.HubProfile, string, error) {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return nil, "", err
	}

	// Migrate legacy flat hub config to profiles map
	if cfg.Hub != nil && cfg.Hub.URL != "" {
		if cfg.Profiles == nil {
			cfg.Profiles = make(map[string]*types.HubProfile)
		}
		if _, exists := cfg.Profiles["default"]; !exists {
			cfg.Profiles["default"] = &types.HubProfile{
				URL:   cfg.Hub.URL,
				Token: cfg.Hub.Token,
			}
			if cfg.ActiveProfile == "" {
				cfg.ActiveProfile = "default"
			}
			cfg.Hub = nil // clear legacy field
			_ = SaveGlobalConfig(cfg)
		}
	}

	// Determine which profile name to use
	name := profileFlag
	if name == "" {
		name = os.Getenv("ELASTICCLAW_PROFILE")
	}
	if name == "" {
		name = cfg.ActiveProfile
	}
	if name == "" {
		name = "default"
	}

	if cfg.Profiles == nil || cfg.Profiles[name] == nil {
		if name == "default" {
			return nil, "", fmt.Errorf("not logged in — run: elasticclaw login --hub <url> --token <token>")
		}
		return nil, "", fmt.Errorf("profile %q not found — run: elasticclaw profile ls", name)
	}

	return cfg.Profiles[name], name, nil
}

// AddHubProfile creates or updates a named hub profile.
func AddHubProfile(name, url, token string) error {
	if !validProfileName.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: use letters, numbers, hyphens, underscores", name)
	}
	cfg, err := LoadGlobalConfig()
	if err != nil {
		cfg = &types.GlobalConfig{}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]*types.HubProfile)
	}
	cfg.Profiles[name] = &types.HubProfile{URL: url, Token: token}
	if cfg.ActiveProfile == "" {
		cfg.ActiveProfile = name
	}
	return SaveGlobalConfig(cfg)
}

// RemoveHubProfile deletes a named hub profile.
func RemoveHubProfile(name string) error {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return err
	}
	if cfg.ActiveProfile == name {
		return fmt.Errorf("cannot remove active profile %q — switch first: elasticclaw profile use <other>", name)
	}
	if cfg.Profiles == nil || cfg.Profiles[name] == nil {
		return fmt.Errorf("profile %q not found", name)
	}
	delete(cfg.Profiles, name)
	return SaveGlobalConfig(cfg)
}

// UseHubProfile sets the active profile.
func UseHubProfile(name string) error {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return err
	}
	if cfg.Profiles == nil || cfg.Profiles[name] == nil {
		return fmt.Errorf("profile %q not found", name)
	}
	cfg.ActiveProfile = name
	return SaveGlobalConfig(cfg)
}

// RenameHubProfile renames a profile.
func RenameHubProfile(oldName, newName string) error {
	if !validProfileName.MatchString(newName) {
		return fmt.Errorf("invalid profile name %q: use letters, numbers, hyphens, underscores", newName)
	}
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return err
	}
	if cfg.Profiles == nil || cfg.Profiles[oldName] == nil {
		return fmt.Errorf("profile %q not found", oldName)
	}
	if cfg.Profiles[newName] != nil {
		return fmt.Errorf("profile %q already exists", newName)
	}
	cfg.Profiles[newName] = cfg.Profiles[oldName]
	delete(cfg.Profiles, oldName)
	if cfg.ActiveProfile == oldName {
		cfg.ActiveProfile = newName
	}
	return SaveGlobalConfig(cfg)
}

// ListHubProfiles returns all hub profiles sorted, with active profile name.
func ListHubProfiles() (profiles map[string]*types.HubProfile, active string, err error) {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return nil, "", err
	}
	// Trigger migration if needed
	if cfg.Hub != nil && cfg.Hub.URL != "" {
		_, _, _ = ResolveHub("")
		cfg, _ = LoadGlobalConfig()
	}
	return cfg.Profiles, cfg.ActiveProfile, nil
}
