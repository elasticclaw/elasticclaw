package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Store manages instance state
type Store struct {
	basePath string
}

// NewStore creates a new state store
func NewStore() (*Store, error) {
	paths, err := config.GetPaths()
	if err != nil {
		return nil, err
	}

	instancesDir := filepath.Join(paths.StateDir, "instances")
	if err := os.MkdirAll(instancesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create instances directory: %w", err)
	}

	return &Store{
		basePath: instancesDir,
	}, nil
}

// Save persists an instance
func (s *Store) Save(instance *types.Instance) error {
	instanceDir := filepath.Join(s.basePath, instance.Name)
	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return fmt.Errorf("failed to create instance directory: %w", err)
	}

	data, err := json.MarshalIndent(instance, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal instance: %w", err)
	}

	metaFile := filepath.Join(instanceDir, "metadata.json")
	if err := os.WriteFile(metaFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write instance metadata: %w", err)
	}

	return nil
}

// Get retrieves an instance by name
func (s *Store) Get(name string) (*types.Instance, error) {
	metaFile := filepath.Join(s.basePath, name, "metadata.json")

	data, err := os.ReadFile(metaFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("instance %q not found", name)
		}
		return nil, fmt.Errorf("failed to read instance metadata: %w", err)
	}

	var instance types.Instance
	if err := json.Unmarshal(data, &instance); err != nil {
		return nil, fmt.Errorf("failed to parse instance metadata: %w", err)
	}

	return &instance, nil
}

// List returns all instances
func (s *Store) List() ([]*types.Instance, error) {
	entries, err := os.ReadDir(s.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read instances directory: %w", err)
	}

	var instances []*types.Instance
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		instance, err := s.Get(entry.Name())
		if err != nil {
			continue // skip invalid instances
		}
		instances = append(instances, instance)
	}

	return instances, nil
}

// Delete removes an instance from state
func (s *Store) Delete(name string) error {
	instanceDir := filepath.Join(s.basePath, name)

	if _, err := os.Stat(instanceDir); os.IsNotExist(err) {
		return fmt.Errorf("instance %q not found", name)
	}

	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	return nil
}

// UpdateStatus updates an instance's status
func (s *Store) UpdateStatus(name string, status types.InstanceStatus) error {
	instance, err := s.Get(name)
	if err != nil {
		return err
	}

	instance.Status = status
	return s.Save(instance)
}

// CreateInstance creates a new instance record
func CreateInstance(name, provider, template, templateVersion string, vars map[string]string) *types.Instance {
	return &types.Instance{
		Name:            name,
		ID:              fmt.Sprintf("%s-%d", name, time.Now().Unix()),
		Provider:        provider,
		Template:        template,
		TemplateVersion: templateVersion,
		StateBackend:    "local",
		Status:          types.StatusPending,
		CreatedAt:       time.Now().UTC(),
		Vars:            vars,
		ProviderMeta:    make(map[string]string),
	}
}
