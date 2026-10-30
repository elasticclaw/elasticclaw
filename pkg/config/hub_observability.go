package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

// ErrNoHubConfig means none of the hub.yaml locations exists.
var ErrNoHubConfig = errors.New("no hub.yaml found; create one first with: elasticclaw hub init")

// HubConfigExists reports whether any hub.yaml location exists.
func HubConfigExists() bool { return loadedHubConfigPath() != "" }

func loadedHubConfigPath() string {
	for _, path := range hubConfigPaths() {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// UpdateHubObservability preserves unrelated YAML fields, including extension fields
// unknown to this version of the hub. It uses the same config resolution as LoadHubConfig.
func UpdateHubObservability(enabled bool, environment *string) error {
	cfg, err := LoadHubConfig()
	if err != nil {
		return err
	}
	if cfg.Observability == nil {
		cfg.Observability = &types.ObservabilityConfig{}
	}
	cfg.Observability.Enabled = enabled
	if enabled {
		cfg.Observability.OTLPEndpoint = "http://localhost:4318"
		if environment != nil {
			cfg.Observability.Environment = *environment
		}
		if cfg.Observability.TracesSampleRatio == nil {
			ratio := 0.25
			cfg.Observability.TracesSampleRatio = &ratio
		}
	}
	// Write back to the file LoadHubConfig read. ActiveHubConfigPath may name a missing
	// $ELASTICCLAW_HUB_CONFIG file, and creating it would shadow the real config.
	path := loadedHubConfigPath()
	if path == "" {
		return ErrNoHubConfig
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if len(data) != 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return err
		}
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("hub config must be a YAML mapping")
	}
	var replacement yaml.Node
	if err := replacement.Encode(cfg.Observability); err != nil {
		return err
	}
	if enabled && environment != nil && *environment == "" {
		replacement.Content = append(replacement.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "environment"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""})
	}
	found := false
	for index := 0; index < len(root.Content); index += 2 {
		if root.Content[index].Value != "observability" {
			continue
		}
		// Preserve optional and future observability settings too.
		old := root.Content[index+1]
		if old.Kind == yaml.MappingNode {
			for n := 0; n < len(replacement.Content); n += 2 {
				key, value := replacement.Content[n], replacement.Content[n+1]
				replaced := false
				for j := 0; j < len(old.Content); j += 2 {
					if old.Content[j].Value == key.Value {
						old.Content[j+1] = value
						replaced = true
						break
					}
				}
				if !replaced {
					old.Content = append(old.Content, key, value)
				}
			}
		} else {
			root.Content[index+1] = &replacement
		}
		found = true
		break
	}
	if !found {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "observability"}, &replacement)
	}
	data, err = yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".hub-observability-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		if err := preserveOwner(file, info); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
