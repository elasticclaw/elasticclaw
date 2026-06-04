# Replicated Provisioning Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Replicated claw provisioning resilient to transient SSH refusal during post-bootstrap setup, and fail the claw instead of marking bootstrap complete when workspace or GitHub setup cannot be completed.

**Architecture:** Keep the change scoped to the Replicated bootstrap path in `pkg/hub/server.go`. Add small retry/readiness helpers, use them for flake staging, bootstrap script execution, template file writes, and GitHub credential helper setup, and keep provider-specific behavior out of shared provider interfaces.

**Tech Stack:** Go, SQLite-backed hub state, Replicated CMX provider, SSH via `golang.org/x/crypto/ssh`, existing `go test` package tests.

---

## Issue Context

Issue: https://github.com/elasticclaw/elasticclaw/issues/271

Observed log from a GitHub issue factory claw:

```text
Warning: failed to write template files: ssh dial: dial tcp 135.181.62.232:36361: connect: connection refused
[bootstrap] warning: cred helper setup failed: ssh dial 135.181.62.232:36361: dial tcp 135.181.62.232:36361: connect: connection refused
Bootstrap complete for claw schemahero/schemahero/1366 (60b96aec)
```

Current behavior:

- `bootstrapReplicated` retries the main bootstrap script 5 times.
- `sshWriteFiles` for final template files retries only 3 times with a fixed 5s sleep and logs a warning after final failure.
- GitHub credential helper setup runs once and logs a warning on failure.
- The hub logs `Bootstrap complete` even when template files or GitHub access failed.

Target behavior:

- Transient SSH dial failures after VM boot get retry with bounded backoff.
- Final template file staging is required; if it cannot complete, the claw transitions to `error`.
- GitHub credential helper setup is required when GitHub Apps access is configured; if it cannot complete, the claw transitions to `error`.
- `Bootstrap complete` is logged only after required post-bootstrap setup succeeds.

Out of scope:

- No changes to Daytona or Exedev provisioning behavior.
- No changes to Replicated API VM creation or polling.
- No new database columns.
- No automatic VM recreation after post-bootstrap failures.

## File Structure

- Modify: `pkg/hub/server.go`
  - Add a Replicated-specific retry helper near `downloadDaytonaConnector` / bootstrap helpers.
  - Add a workspace readiness verification helper for Replicated template files.
  - Update `bootstrapReplicated` to use the helper and fail hard after exhausted retries.
- Create: `pkg/hub/replicated_resilience_test.go`
  - Unit tests for retry helper behavior.
  - Unit tests for Replicated workspace readiness command generation.
- Test: existing `pkg/hub/bootstrap_test.go`
  - Keep existing generated script tests passing; no script behavior should change for this issue.

Existing helpers used:

- `shellQuote(s string) string` exists in `pkg/hub/bootstrap.go` and is available to `pkg/hub` tests because they share package `hub`.
- `formatRetryDelay(d time.Duration) string` exists in `pkg/hub/server.go` and formats retry durations for bootstrap status messages.
- `sanitizeBootstrapError(err error) string` exists in `pkg/hub/server.go` and truncates/sanitizes bootstrap errors for logs and diagnostics.
- `sshRun`, `sshWriteFiles`, `setBootstrapStatus`, and `stopAgentWithReason` are existing `Server` methods in `pkg/hub/server.go`.

## Acceptance Criteria

- A temporary `connect: connection refused` from Replicated SSH during flake staging, template file writes, or credential helper setup is retried before the claw fails.
- If template files still cannot be written after retries, `stopAgentWithReason` is called with a sanitized reason containing `Bootstrap failed: could not write workspace files`.
- If GitHub Apps are configured and credential helper setup still cannot complete after retries, `stopAgentWithReason` is called with a sanitized reason containing `Bootstrap failed: could not configure GitHub credentials`.
- `Bootstrap complete for claw ...` is reached only after required workspace files, checkpoint restore, and GitHub credential setup have succeeded.
- Retry status is visible via `bootstrap_status`, using labels like `Retrying workspace file write in 10s`.
- Unit tests pass with `go test ./pkg/hub`.

---

### Task 1: Add Replicated Retry Helper Tests

**Files:**
- Create: `pkg/hub/replicated_resilience_test.go`
- Modify: none
- Test: `pkg/hub/replicated_resilience_test.go`

- [ ] **Step 1: Create failing tests for bounded retry behavior**

Create `pkg/hub/replicated_resilience_test.go` with this content:

```go
package hub

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetryReplicatedBootstrapStepSucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0
	var slept []time.Duration

	err := retryReplicatedBootstrapStep(nil, "", replicatedBootstrapRetryOptions{
		Label:      "Writing workspace files",
		RetryLabel: "Retrying workspace file write",
		Attempts:   4,
		Delays:     []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second},
		Sleep: func(d time.Duration) {
			slept = append(slept, d)
		},
		Run: func() error {
			attempts++
			if attempts < 3 {
				return errors.New("ssh dial: connect: connection refused")
			}
			return nil
		},
	})

	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if len(slept) != 2 || slept[0] != 5*time.Second || slept[1] != 10*time.Second {
		t.Fatalf("unexpected retry delays: %#v", slept)
	}
}

func TestRetryReplicatedBootstrapStepReturnsSanitizedFinalError(t *testing.T) {
	attempts := 0

	err := retryReplicatedBootstrapStep(nil, "", replicatedBootstrapRetryOptions{
		Label:      "Configuring GitHub credentials",
		RetryLabel: "Retrying GitHub credential setup",
		Attempts:   3,
		Delays:     []time.Duration{5 * time.Second, 10 * time.Second},
		Sleep:      func(time.Duration) {},
		Run: func() error {
			attempts++
			return errors.New(strings.Repeat("ssh dial: connect: connection refused\n", 100))
		},
	})

	if err == nil {
		t.Fatal("expected final error")
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if !strings.Contains(err.Error(), "Configuring GitHub credentials failed after 3 attempts") {
		t.Fatalf("expected labeled final error, got %q", err.Error())
	}
	if len(err.Error()) > 1400 {
		t.Fatalf("expected sanitized/truncated error, got %d bytes", len(err.Error()))
	}
}

func TestReplicatedWorkspaceReadinessCommandQuotesPaths(t *testing.T) {
	files := map[string]string{
		"AGENTS.md":               "agent instructions",
		"dir/BOOTSTRAP notes.md":  "notes",
		"quote'file.md":           "quoted path",
		"empty-file-is-allowed.md": "",
	}

	cmd := replicatedWorkspaceReadinessCommand("$HOME/.openclaw/workspace", files)

	for name := range files {
		if !strings.Contains(cmd, shellDoubleQuote("$HOME/.openclaw/workspace/"+name)) {
			t.Fatalf("expected quoted path for %q in command:\n%s", name, cmd)
		}
	}
	if strings.Contains(cmd, " -s ") {
		t.Fatalf("readiness must use test -e so empty files are allowed:\n%s", cmd)
	}
	if !strings.Contains(cmd, "test -e") {
		t.Fatalf("expected test -e checks in command:\n%s", cmd)
	}
}
```

- [ ] **Step 2: Run tests and verify they fail**

Run:

```bash
go test ./pkg/hub -run 'TestRetryReplicatedBootstrapStep|TestReplicatedWorkspaceReadinessCommand' -count=1
```

Expected: FAIL because `retryReplicatedBootstrapStep`, `replicatedBootstrapRetryOptions`, and `replicatedWorkspaceReadinessCommand` do not exist yet.

- [ ] **Step 3: Commit failing tests**

Run:

```bash
git add pkg/hub/replicated_resilience_test.go
git commit -m "test: cover replicated bootstrap retry behavior"
```

---

### Task 2: Implement Replicated Retry and Readiness Helpers

**Files:**
- Modify: `pkg/hub/server.go`
- Test: `pkg/hub/replicated_resilience_test.go`

- [ ] **Step 1: Add helper types and functions**

In `pkg/hub/server.go`, add this code near the existing bootstrap helper functions, immediately before `func (s *Server) setBootstrapStatus(clawID, status string)`:

```go
type replicatedBootstrapRetryOptions struct {
	Label      string
	RetryLabel string
	Attempts   int
	Delays     []time.Duration
	Sleep      func(time.Duration)
	Run        func() error
}

func retryReplicatedBootstrapStep(s *Server, clawID string, opts replicatedBootstrapRetryOptions) error {
	if opts.Attempts < 1 {
		opts.Attempts = 1
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.RetryLabel == "" {
		opts.RetryLabel = "Retrying " + strings.ToLower(opts.Label)
	}

	var lastErr error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if attempt > 1 {
			delay := replicatedBootstrapDelay(opts.Delays, attempt-2)
			if s != nil && clawID != "" {
				s.setBootstrapStatus(clawID, fmt.Sprintf("%s in %s", opts.RetryLabel, formatRetryDelay(delay)))
			}
			log.Printf("[bootstrap] %s retry %d/%d in %s...", opts.Label, attempt, opts.Attempts, delay)
			opts.Sleep(delay)
		}
		if s != nil && clawID != "" {
			s.setBootstrapStatus(clawID, opts.Label)
		}
		if err := opts.Run(); err != nil {
			lastErr = err
			log.Printf("[bootstrap] %s attempt %d/%d failed: %s", opts.Label, attempt, opts.Attempts, sanitizeBootstrapError(err))
			continue
		}
		return nil
	}

	return fmt.Errorf("%s failed after %d attempts: %s", opts.Label, opts.Attempts, sanitizeBootstrapError(lastErr))
}

func replicatedBootstrapDelay(delays []time.Duration, idx int) time.Duration {
	if len(delays) == 0 {
		return 5 * time.Second
	}
	if idx < len(delays) {
		return delays[idx]
	}
	return delays[len(delays)-1]
}

func replicatedWorkspaceReadinessCommand(dir string, files map[string]string) string {
	if len(files) == 0 {
		return "true"
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("set -e\n")
	for _, name := range names {
		path := strings.TrimRight(dir, "/") + "/" + name
		b.WriteString("test -e ")
		b.WriteString(shellDoubleQuote(path))
		b.WriteString(" || { echo ")
		b.WriteString(shellQuote("missing workspace file: " + name))
		b.WriteString("; exit 1; }\n")
	}
	b.WriteString("echo 'workspace files verified'\n")
	return b.String()
}

func shellDoubleQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if i == 0 && strings.HasPrefix(s, "$HOME") && (len(s) == len("$HOME") || s[len("$HOME")] == '/') {
			b.WriteString("$HOME")
			i += len("$HOME") - 1
			continue
		}
		switch s[i] {
		case '\\', '"', '`', '$':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}
```

Also add `sort` to the existing `pkg/hub/server.go` import list:

```go
import (
	"sort"
)
```

Keep the real import block merged with the existing imports; do not create a second import block.

- [ ] **Step 2: Run focused tests**

Run:

```bash
go test ./pkg/hub -run 'TestRetryReplicatedBootstrapStep|TestReplicatedWorkspaceReadinessCommand' -count=1
```

Expected: PASS.

- [ ] **Step 3: Commit helper implementation**

Run:

```bash
git add pkg/hub/server.go pkg/hub/replicated_resilience_test.go
git commit -m "fix: add replicated bootstrap retry helper"
```

---

### Task 3: Use Retries for Replicated Post-Bootstrap Setup

**Files:**
- Modify: `pkg/hub/server.go`
- Test: `pkg/hub/replicated_resilience_test.go`

- [ ] **Step 1: Define retry delays in `bootstrapReplicated`**

In `pkg/hub/server.go`, inside `bootstrapReplicated`, after `sshHost := fmt.Sprintf("%s:%d", vm.DirectSSHEndpoint, vm.DirectSSHPort)`, add:

```go
	replicatedSSHDelays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
	}
```

- [ ] **Step 2: Replace flake staging direct write with retry**

Replace the current flake staging block:

```go
	if flakeFiles := templateFlakeFiles(files); len(flakeFiles) > 0 {
		s.setBootstrapStatus(clawID, "Staging Nix flake")
		if err := s.sshWriteFiles(sshUser, sshHost, "$HOME/workspace", flakeFiles); err != nil {
			log.Printf("[bootstrap] failed to stage flake before bootstrap for claw %s: %v", clawID[:8], err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not stage flake files: %s", sanitizeBootstrapError(err)), false)
			return
		}
	}
```

with:

```go
	if flakeFiles := templateFlakeFiles(files); len(flakeFiles) > 0 {
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Staging Nix flake",
			RetryLabel: "Retrying Nix flake staging",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshWriteFiles(sshUser, sshHost, "$HOME/workspace", flakeFiles)
			},
		}); err != nil {
			log.Printf("[bootstrap] failed to stage flake before bootstrap for claw %s: %v", clawID[:8], err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not stage flake files: %s", sanitizeBootstrapError(err)), false)
			return
		}
	}
```

- [ ] **Step 3: Replace main bootstrap script retry loop with helper**

Replace:

```go
	var sshErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			s.setBootstrapStatus(clawID, "Retrying sandbox bootstrap in 10s")
			log.Printf("Bootstrap retry %d/5 for claw %s in 10s...", attempt, clawName)
			time.Sleep(10 * time.Second)
		}
		s.setBootstrapStatus(clawID, "Preparing ElasticClaw connector")
		if sshErr = s.sshRun(sshUser, sshHost, script); sshErr == nil {
			break
		}
		log.Printf("Bootstrap attempt %d/5 failed: %v", attempt, sanitizeBootstrapError(sshErr))
	}
	if sshErr != nil {
		log.Printf("Bootstrap failed for claw %s after 5 attempts: %v", clawID, sanitizeBootstrapError(sshErr))
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed after 5 attempts: %s", sanitizeBootstrapError(sshErr)), false)
		return
	}
```

with:

```go
	if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
		Label:      "Preparing ElasticClaw connector",
		RetryLabel: "Retrying sandbox bootstrap",
		Attempts:   5,
		Delays:     []time.Duration{10 * time.Second},
		Run: func() error {
			return s.sshRun(sshUser, sshHost, script)
		},
	}); err != nil {
		log.Printf("Bootstrap failed for claw %s: %v", clawID, err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: %s", sanitizeBootstrapError(err)), false)
		return
	}
```

- [ ] **Step 4: Make final template file writes required**

Replace the current template file write retry block:

```go
	if len(files) > 0 {
		fileNames := make([]string, 0, len(files))
		for k := range files {
			fileNames = append(fileNames, k)
		}
		log.Printf("[bootstrap] writing %d template files for claw %s: %v", len(files), clawName, fileNames)
		for attempt := 1; attempt <= 3; attempt++ {
			if err := s.sshWriteFiles(sshUser, sshHost, "$HOME/.openclaw/workspace", files); err == nil {
				log.Printf("Template files written for claw %s", clawName)
				break
			} else if attempt == 3 {
				log.Printf("Warning: failed to write template files: %v", err)
			} else {
				time.Sleep(5 * time.Second)
			}
		}
	}
```

with:

```go
	if len(files) > 0 {
		fileNames := make([]string, 0, len(files))
		for k := range files {
			fileNames = append(fileNames, k)
		}
		sort.Strings(fileNames)
		log.Printf("[bootstrap] writing %d template files for claw %s: %v", len(files), clawName, fileNames)
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Writing workspace files",
			RetryLabel: "Retrying workspace file write",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshWriteFiles(sshUser, sshHost, "$HOME/.openclaw/workspace", files)
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not write workspace files: %s", sanitizeBootstrapError(err)), false)
			return
		}
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Verifying workspace files",
			RetryLabel: "Retrying workspace file verification",
			Attempts:   3,
			Delays:     []time.Duration{2 * time.Second, 5 * time.Second},
			Run: func() error {
				return s.sshRun(sshUser, sshHost, replicatedWorkspaceReadinessCommand("$HOME/.openclaw/workspace", files))
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: workspace files incomplete: %s", sanitizeBootstrapError(err)), false)
			return
		}
		log.Printf("Template files written for claw %s", clawName)
	}
```

- [ ] **Step 5: Make GitHub credential helper setup required when configured**

Replace:

```go
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := s.sshRun(sshUser, sshHost, credHelper); err != nil {
			log.Printf("[bootstrap] warning: cred helper setup failed: %v", err)
		} else {
			log.Printf("[bootstrap] GitHub credential helper installed for claw %s", clawName)
		}
	}
```

with:

```go
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Configuring GitHub credentials",
			RetryLabel: "Retrying GitHub credential setup",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshRun(sshUser, sshHost, credHelper)
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not configure GitHub credentials: %s", sanitizeBootstrapError(err)), false)
			return
		}
		log.Printf("[bootstrap] GitHub credential helper installed for claw %s", clawName)
	}
```

- [ ] **Step 6: Run focused tests**

Run:

```bash
go test ./pkg/hub -run 'TestRetryReplicatedBootstrapStep|TestReplicatedWorkspaceReadinessCommand|TestBootstrapScript' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit Replicated bootstrap behavior change**

Run:

```bash
git add pkg/hub/server.go pkg/hub/replicated_resilience_test.go
git commit -m "fix: harden replicated post-bootstrap setup"
```

---

### Task 4: Verify Full Hub Package

**Files:**
- Modify: none
- Test: `pkg/hub`

- [ ] **Step 1: Run full hub tests**

Run:

```bash
go test ./pkg/hub
```

Expected: PASS.

- [ ] **Step 2: Run provider-specific compile/test surface**

Run:

```bash
go test ./pkg/provider/replicated ./pkg/provider/daytona ./pkg/provider/exedev
```

Expected: PASS.

- [ ] **Step 3: Run a full Go test if time allows**

Run:

```bash
go test ./...
```

Expected: PASS. If this fails outside the touched packages, capture the failing package and error in the handoff.

- [ ] **Step 4: Manual verification checklist for a Replicated GitHub issue factory**

Use a staging hub configured with Replicated and GitHub Apps, then create a claw from a GitHub issue factory.

Expected observations:

```text
[bootstrap] Writing workspace files attempt 1/6 failed: ...
[bootstrap] Writing workspace files retry 2/6 in 5s...
[bootstrap] Configuring GitHub credentials retry 2/6 in 5s...
Template files written for claw ...
[bootstrap] GitHub credential helper installed for claw ...
Bootstrap complete for claw ...
```

If SSH remains unreachable through all attempts, expected hub state:

```text
status = error
bootstrap_diagnostic contains "Bootstrap failed: could not write workspace files" or "Bootstrap failed: could not configure GitHub credentials"
```

- [ ] **Step 5: Commit final verification note if docs are updated**

Only run this if a docs or changelog note is added during execution:

```bash
git add docs/superpowers/plans/2026-06-04-replicated-provisioning-reliability.md
git commit -m "docs: document replicated provisioning reliability plan"
```

---

## Self-Review

Spec coverage:

- Issue log's template file failure is covered by Task 3 Step 4.
- Issue log's GitHub credential helper failure is covered by Task 3 Step 5.
- The misleading `Bootstrap complete` log is covered by failing hard before the final log in Task 3.
- Transient Replicated SSH refusal is covered by the retry helper in Task 2 and its use in Task 3.

Placeholder scan:

- No deferred-work markers or vague "handle errors" steps remain.
- Every code-changing step includes the concrete code to insert or replace.

Type consistency:

- Tests use `replicatedBootstrapRetryOptions`, `retryReplicatedBootstrapStep`, and `replicatedWorkspaceReadinessCommand`, all defined in Task 2.
- The implementation uses existing helpers `sanitizeBootstrapError`, `formatRetryDelay`, `shellQuote`, `sshRun`, `sshWriteFiles`, and `stopAgentWithReason`; the plan adds `shellDoubleQuote` for remote paths that must preserve `$HOME` expansion.
