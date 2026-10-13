package v2

// ExecRunConfig is the v2 effect payload for exec.run.
type ExecRunConfig struct {
	Command string `json:"command" yaml:"command"`
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// ExecRunReceipt is emitted by the bridge when a command finishes.
type ExecRunReceipt struct {
	ExitCode  int    `json:"exit_code"`
	Succeeded bool   `json:"succeeded"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DependencyUpdateConfig is the v2 effect payload for dependency.update.
// It mirrors v1's DependencyUpdatesAction fields (excluding output and
// continue_on_error, which are deliberately not carried forward).
type DependencyUpdateConfig struct {
	Ecosystems       []string `json:"ecosystems" yaml:"ecosystems"`
	Paths            []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	ExcludePaths     []string `json:"exclude_paths,omitempty" yaml:"exclude_paths,omitempty"`
	Grouping         string   `json:"grouping,omitempty" yaml:"grouping,omitempty"`
	IncludeMajor     bool     `json:"include_major,omitempty" yaml:"include_major,omitempty"`
	SeparateMajor    *bool    `json:"separate_major,omitempty" yaml:"separate_major,omitempty"`
	SeparateSecurity *bool    `json:"separate_security,omitempty" yaml:"separate_security,omitempty"`
	SeparateRuntime  *bool    `json:"separate_runtime,omitempty" yaml:"separate_runtime,omitempty"`
	Allow            []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Ignore           []string `json:"ignore,omitempty" yaml:"ignore,omitempty"`
	Timeout          string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// DependencyUpdateReceipt is emitted by the bridge when a dependency update
// pass finishes. It carries the structured JSON produced by the dependency
// update script, plus a top-level succeeded flag and a human-readable error.
type DependencyUpdateReceipt struct {
	Ecosystems   []string        `json:"ecosystems,omitempty"`
	Manifests    []any             `json:"manifests,omitempty"`
	Updates      []any             `json:"updates,omitempty"`
	Commands     []any             `json:"commands,omitempty"`
	FilesChanged []string        `json:"files_changed,omitempty"`
	Succeeded    bool            `json:"succeeded"`
	Error        string          `json:"error,omitempty"`
}
