package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types/convert"
	"github.com/spf13/cobra"
)

// shared convert flags bound by workspace/workflow convert commands
type convertFlags struct {
	to         string
	from       string
	output     string
	inPlace    bool
	workspace  string // workflow convert only: path to workspace config for pair validation
	noValidate bool
}

func newConvertFlags() *convertFlags {
	return &convertFlags{to: "2"}
}

func bindConvertFlags(cmd *cobra.Command, f *convertFlags, includeWorkspace bool) {
	cmd.Flags().StringVar(&f.to, "to", "2", "target schema version (e.g. 2; future: 3)")
	cmd.Flags().StringVar(&f.from, "from", "", "source schema version (default: auto-detect from document)")
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "write converted YAML to this file (default: stdout)")
	cmd.Flags().BoolVar(&f.inPlace, "in-place", false, "overwrite the input file (or elasticclaw-config.yaml for a workspace directory)")
	cmd.Flags().BoolVar(&f.noValidate, "no-validate", false, "skip post-conversion schema validation")
	if includeWorkspace {
		cmd.Flags().StringVar(&f.workspace, "workspace", "", "path to a workspace v2 YAML or directory (pair-validate workflow refs)")
	}
}

func workspaceConvertCmd() *cobra.Command {
	f := newConvertFlags()
	cmd := &cobra.Command{
		Use:   "convert <path>",
		Short: "Convert a workspace document between schema versions",
		Long: `Convert a workspace YAML document to another schema version.

PATH may be a workspace directory (uses elasticclaw-config.yaml) or a YAML file.

By default converts to schema version 2. Output is written to stdout unless
--output or --in-place is set. Conversion is intentionally lossy where v1
concepts have no safe v2 equivalent; warnings are printed to stderr.

Examples:
  elasticclaw workspace convert .elasticclaw/workspaces/my-ws
  elasticclaw workspace convert ./elasticclaw-config.yaml --to 2 -o v2.yaml
  elasticclaw workspace convert ./ws --in-place
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConvert(convert.KindWorkspace, args[0], f)
		},
	}
	bindConvertFlags(cmd, f, false)
	return cmd
}

func workflowConvertCmd() *cobra.Command {
	f := newConvertFlags()
	cmd := &cobra.Command{
		Use:   "convert <path>",
		Short: "Convert a workflow document between schema versions",
		Long: `Convert a workflow YAML document to another schema version.

PATH is a workflow YAML file (or a directory of *.yaml workflows — each file
is converted independently when --in-place is set; otherwise a single file is required).

By default converts to schema version 2. Subjective v1 controls such as
message_contains, [DONE], and [READY_TO_COMMIT] are never rewritten as trusted
v2 evidence; they are reported as warnings for manual migration.

Examples:
  elasticclaw workflow convert examples/workflows/github-issue.yaml
  elasticclaw workflow convert ./wf.yaml --to 2 --workspace ./ws -o wf.v2.yaml
  elasticclaw workflow convert ./github-issue.yaml --in-place
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConvert(convert.KindWorkflow, args[0], f)
		},
	}
	bindConvertFlags(cmd, f, true)
	return cmd
}

func runConvert(kind convert.Kind, path string, f *convertFlags) error {
	if f.inPlace && f.output != "" {
		return fmt.Errorf("use only one of --in-place or --output")
	}

	inputPath, data, err := readConvertInput(kind, path)
	if err != nil {
		return err
	}

	opts := convert.Options{
		From: f.from,
		To:   f.to,
	}
	if f.noValidate {
		v := false
		opts.Validate = &v
	}
	if kind == convert.KindWorkflow && strings.TrimSpace(f.workspace) != "" {
		wsPath, wsData, err := readConvertInput(convert.KindWorkspace, f.workspace)
		if err != nil {
			return fmt.Errorf("--workspace: %w", err)
		}
		_ = wsPath
		opts.WorkspaceYAML = wsData
	}

	result, err := convert.Convert(kind, data, opts)
	if err != nil {
		return err
	}

	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "converted %s %s → %s (%d warning(s))\n",
			kind, result.From, result.To, len(result.Warnings))
	}

	switch {
	case f.inPlace:
		if err := os.WriteFile(inputPath, result.Output, 0644); err != nil {
			return fmt.Errorf("write %s: %w", inputPath, err)
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "wrote %s\n", inputPath)
		}
	case f.output != "":
		if dir := filepath.Dir(f.output); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("mkdir for %s: %w", f.output, err)
			}
		}
		if err := os.WriteFile(f.output, result.Output, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.output, err)
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "wrote %s\n", f.output)
		}
	default:
		if _, err := os.Stdout.Write(result.Output); err != nil {
			return err
		}
	}
	return nil
}

// readConvertInput resolves a path to the YAML file bytes to convert.
// For workspaces, a directory resolves to elasticclaw-config.yaml (or workspace.yaml).
func readConvertInput(kind convert.Kind, path string) (filePath string, data []byte, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if info.IsDir() {
		if kind != convert.KindWorkspace {
			return "", nil, fmt.Errorf("%s convert expects a YAML file, got directory %q", kind, path)
		}
		for _, name := range []string{"elasticclaw-config.yaml", "workspace.yaml"} {
			candidate := filepath.Join(path, name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				data, err := os.ReadFile(candidate)
				if err != nil {
					return "", nil, err
				}
				return candidate, data, nil
			}
		}
		return "", nil, fmt.Errorf("no elasticclaw-config.yaml (or workspace.yaml) in %s", path)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	return path, data, nil
}
