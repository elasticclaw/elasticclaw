package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

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

	cfg := &types.GlobalConfig{
		Catalogs: []string{"https://catalog.elasticclaw.dev/images.yaml"},
	}

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

// GetActiveProfile returns the currently active profile
func GetActiveProfile() (*types.Profile, error) {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return nil, err
	}

	if cfg.ActiveProfile == "" {
		// Return a default profile
		return &types.Profile{
			Name:     "default",
			Provider: "daytona",
			State:    "local",
		}, nil
	}

	return LoadProfile(cfg.ActiveProfile)
}
