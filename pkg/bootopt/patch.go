package bootopt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PatchApplier handles applying and reverting diffs.
type PatchApplier struct {
	RepoRoot string
}

// NewPatchApplier creates a patch applier for the given repo.
func NewPatchApplier(repoRoot string) *PatchApplier {
	return &PatchApplier{RepoRoot: repoRoot}
}

// Apply attempts to apply a diff. Returns rollback function on success.
func (pa *PatchApplier) Apply(diff string) (rollback func() error, err error) {
	// Write diff to temp file
	diffFile, err := os.CreateTemp("", "bootopt-*.diff")
	if err != nil {
		return nil, fmt.Errorf("create temp diff: %w", err)
	}
	defer os.Remove(diffFile.Name())

	if _, err := diffFile.WriteString(diff); err != nil {
		diffFile.Close()
		return nil, fmt.Errorf("write diff: %w", err)
	}
	diffFile.Close()

	// Try git apply
	cmd := exec.Command("git", "apply", "--check", diffFile.Name())
	cmd.Dir = pa.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git apply --check failed: %w\n%s", err, string(out))
	}

	cmd = exec.Command("git", "apply", diffFile.Name())
	cmd.Dir = pa.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git apply failed: %w\n%s", err, string(out))
	}

	// Build rollback function
	rollback = func() error {
		cmd := exec.Command("git", "checkout", "--", ".")
		cmd.Dir = pa.RepoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("rollback failed: %w\n%s", err, string(out))
		}
		return nil
	}

	return rollback, nil
}

// VerifyBuild runs go build to ensure the patch doesn't break compilation.
func (pa *PatchApplier) VerifyBuild() error {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = pa.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build failed: %w\n%s", err, string(out))
	}
	return nil
}

// VerifyTests runs the specified test pattern.
// GetFileContent reads a file relative to repo root.
func (pa *PatchApplier) GetFileContent(relPath string) (string, error) {
	path := filepath.Join(pa.RepoRoot, relPath)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GetCurrentCode returns key files for LLM context.
func (pa *PatchApplier) GetCurrentCode(files []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, f := range files {
		content, err := pa.GetFileContent(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		result[f] = content
	}
	return result, nil
}

// Commit keeps the current changes (after successful hypothesis).
func (pa *PatchApplier) Commit(message string) error {
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = pa.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w\n%s", err, string(out))
	}

	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = pa.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		// Might be nothing to commit
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		resetCmd := exec.Command("git", "reset", "--", ".")
		resetCmd.Dir = pa.RepoRoot
		resetCmd.Run()
		return fmt.Errorf("git commit: %w\n%s", err, string(out))
	}
	return nil
}
