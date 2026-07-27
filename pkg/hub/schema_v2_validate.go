package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// validateWorkspaceDocumentAtStore rejects invalid workspace v2 YAML at the
// persistence boundary. Non-v2 documents are left to WorkspaceConfig.Validate.
func validateWorkspaceDocumentAtStore(data []byte) error {
	version, err := v2.DetectSchemaVersion(data)
	if err != nil {
		return fmt.Errorf("workspace schema: %w", err)
	}
	if !v2.IsV2(version) {
		return nil
	}
	if _, err := v2.ParseAndValidateWorkspace(data); err != nil {
		return fmt.Errorf("invalid workspace v2: %w", err)
	}
	return nil
}

// validateWorkflowDocumentAtStore rejects invalid workflow v2 YAML (and invalid
// v2 workspace pairs) at the persistence boundary. Non-v2 documents are left to
// the existing NormalizeWorkflowConfig + WorkflowConfig.Validate path.
func validateWorkflowDocumentAtStore(workflowData, workspaceYAML []byte) error {
	version, err := v2.DetectSchemaVersion(workflowData)
	if err != nil {
		return fmt.Errorf("workflow schema: %w", err)
	}
	if !v2.IsV2(version) {
		return nil
	}

	// Prefer authored workspace YAML when it is v2 for full pair validation.
	if len(strings.TrimSpace(string(workspaceYAML))) > 0 && workspaceYAMLIsV2(workspaceYAML) {
		if _, _, err := v2.ParseAndValidateWorkflowPair(workflowData, workspaceYAML); err != nil {
			return fmt.Errorf("invalid workflow v2 pair: %w", err)
		}
		return nil
	}

	// Workflow is v2 but workspace is not v2: require structural workflow validity,
	// then refuse the incomplete pair (v2 workflow requires v2 workspace).
	if _, err := v2.ParseAndValidateWorkflow(workflowData); err != nil {
		return fmt.Errorf("invalid workflow v2: %w", err)
	}
	return fmt.Errorf("invalid workflow v2 pair: workflow schema v2 requires a workspace schema v2")
}

func workspaceYAMLIsV2(data []byte) bool {
	version, err := v2.DetectSchemaVersion(data)
	return err == nil && v2.IsV2(version)
}

// readExternalWorkspaceYAML returns the authored elasticclaw-config.yaml bytes.
func readExternalWorkspaceYAML(name string) ([]byte, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(workspacesDir(), name, "elasticclaw-config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		legacy := filepath.Join(workspacesDir(), name, "workspace.yaml")
		data, err = os.ReadFile(legacy)
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}
