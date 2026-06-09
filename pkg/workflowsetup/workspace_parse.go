package workflowsetup

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

// ParsedWorkspaceConfig keeps both workspace views needed by setup validation.
// RawConfig is preserved for later simulations that need the authored bytes.
type ParsedWorkspaceConfig struct {
	RawConfig string
	Workspace *types.WorkspaceConfig
	Template  *types.TemplateConfig
}

// ParseWorkspaceConfig parses workspace YAML as both the workspace schema and
// the template-compatible schema used by legacy config loading.
func ParseWorkspaceConfig(raw string) (*ParsedWorkspaceConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	workspace := &types.WorkspaceConfig{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	if err := dec.Decode(workspace); err != nil && err != io.EOF {
		return nil, fmt.Errorf("invalid workspace config: %w", err)
	}

	template, err := config.ParseTemplateConfig([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid template-compatible workspace config: %w", err)
	}

	return &ParsedWorkspaceConfig{
		RawConfig: raw,
		Workspace: workspace,
		Template:  template,
	}, nil
}
