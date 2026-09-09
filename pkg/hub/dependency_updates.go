package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
)

const (
	defaultDependencyUpdatesOutput  = "dependency_updates"
	defaultDependencyUpdatesTimeout = 30 * time.Minute
)

type dependencyUpdatesConfig struct {
	Ecosystems       []string `json:"ecosystems"`
	Paths            []string `json:"paths"`
	ExcludePaths     []string `json:"exclude_paths"`
	Grouping         string   `json:"grouping"`
	IncludeMajor     bool     `json:"include_major"`
	SeparateMajor    bool     `json:"separate_major"`
	SeparateSecurity bool     `json:"separate_security"`
	SeparateRuntime  bool     `json:"separate_runtime"`
	Allow            []string `json:"allow"`
	Ignore           []string `json:"ignore"`
}

type dependencyUpdateManifest struct {
	Ecosystem string   `json:"ecosystem"`
	Path      string   `json:"path"`
	Lockfiles []string `json:"lockfiles,omitempty"`
}

func normalizeDependencyUpdatesConfig(action pipeline.DependencyUpdatesAction) dependencyUpdatesConfig {
	cfg := dependencyUpdatesConfig{
		Ecosystems:       cleanStringList(action.Ecosystems),
		Paths:            cleanStringList(action.Paths),
		ExcludePaths:     cleanStringList(action.ExcludePaths),
		Grouping:         strings.TrimSpace(action.Grouping),
		IncludeMajor:     action.IncludeMajor,
		SeparateMajor:    boolDefault(action.SeparateMajor, true),
		SeparateSecurity: boolDefault(action.SeparateSecurity, true),
		SeparateRuntime:  boolDefault(action.SeparateRuntime, true),
		Allow:            cleanStringList(action.Allow),
		Ignore:           cleanStringList(action.Ignore),
	}
	if len(cfg.Ecosystems) == 0 {
		cfg.Ecosystems = []string{"auto"}
	}
	if len(cfg.Paths) == 0 {
		cfg.Paths = []string{"."}
	}
	if cfg.Grouping == "" {
		cfg.Grouping = "all"
	}
	if len(cfg.Allow) == 0 {
		cfg.Allow = []string{"*"}
	}
	return cfg
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func dependencyUpdatesOutputName(action pipeline.DependencyUpdatesAction) string {
	if strings.TrimSpace(action.Output) != "" {
		return strings.TrimSpace(action.Output)
	}
	return defaultDependencyUpdatesOutput
}

func dependencyUpdatesTimeout(action pipeline.DependencyUpdatesAction) (time.Duration, error) {
	if strings.TrimSpace(action.Timeout) == "" {
		return defaultDependencyUpdatesTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(action.Timeout))
	if err != nil {
		return 0, fmt.Errorf("invalid dependency_updates timeout %q: %w", action.Timeout, err)
	}
	return timeout, nil
}

func cleanStringList(values []string) []string {
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

// dependencyUpdatesConfigured reports whether the dependency update stage is
// actually configured to do work. ContinueOnError is intentionally excluded:
// the parser already sets Enabled when the dependency_updates block is present,
// and ContinueOnError only controls whether the pipeline continues after a failure.
func dependencyUpdatesConfigured(action pipeline.DependencyUpdatesAction) bool {
	return action.Enabled ||
		len(action.Ecosystems) > 0 ||
		len(action.Paths) > 0 ||
		len(action.ExcludePaths) > 0 ||
		strings.TrimSpace(action.Grouping) != "" ||
		action.IncludeMajor ||
		action.SeparateMajor != nil ||
		action.SeparateSecurity != nil ||
		action.SeparateRuntime != nil ||
		len(action.Allow) > 0 ||
		len(action.Ignore) > 0 ||
		strings.TrimSpace(action.Output) != "" ||
		strings.TrimSpace(action.Timeout) != ""
}

func isExcludedPath(relPath string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if matched, _ := path.Match(p, relPath); matched {
			return true
		}
		if !strings.ContainsAny(p, "*?") {
			if relPath == p || strings.HasPrefix(relPath, p+"/") {
				return true
			}
		}
	}
	return false
}

func discoverDependencyUpdateManifests(root string, cfg dependencyUpdatesConfig) ([]dependencyUpdateManifest, error) {
	ecosystems := dependencyUpdateEcosystems(cfg.Ecosystems)
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var manifests []dependencyUpdateManifest
	seen := map[string]bool{}
	for _, rel := range cfg.Paths {
		base, err := filepath.Abs(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		baseRel, err := filepath.Rel(root, base)
		if err != nil || baseRel == ".." || strings.HasPrefix(baseRel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("dependency update path %q escapes workspace", rel)
		}
		info, err := os.Stat(base)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			base = filepath.Dir(base)
		}
		err = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relPath = filepath.ToSlash(relPath)
			if d.IsDir() {
				if shouldSkipDependencyUpdateDir(d.Name()) {
					if path == base {
						return nil
					}
					return filepath.SkipDir
				}
				if isExcludedPath(relPath, cfg.ExcludePaths) {
					return filepath.SkipDir
				}
				return nil
			}
			if isExcludedPath(relPath, cfg.ExcludePaths) {
				return nil
			}
			name := d.Name()
			dir := filepath.Dir(path)
			switch {
			case ecosystems["go"] && name == "go.mod":
				lock := filepath.Join(dir, "go.sum")
				lockfiles := existingRelFiles(root, lock)
				key := "go:" + relPath
				if !seen[key] {
					seen[key] = true
					manifests = append(manifests, dependencyUpdateManifest{Ecosystem: "go", Path: relPath, Lockfiles: lockfiles})
				}
			case ecosystems["npm"] && name == "package.json":
				lockfiles := existingRelFiles(root,
					filepath.Join(dir, "package-lock.json"),
					filepath.Join(dir, "npm-shrinkwrap.json"),
				)
				key := "npm:" + relPath
				if !seen[key] {
					seen[key] = true
					manifests = append(manifests, dependencyUpdateManifest{Ecosystem: "npm", Path: relPath, Lockfiles: lockfiles})
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(manifests, func(i, j int) bool {
		if manifests[i].Ecosystem == manifests[j].Ecosystem {
			return manifests[i].Path < manifests[j].Path
		}
		return manifests[i].Ecosystem < manifests[j].Ecosystem
	})
	return manifests, nil
}

func dependencyUpdateEcosystems(values []string) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "auto" {
			result["go"] = true
			result["npm"] = true
			continue
		}
		result[value] = true
	}
	return result
}

func shouldSkipDependencyUpdateDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".next", "dist", "build":
		return true
	default:
		return false
	}
}

func existingRelFiles(root string, paths ...string) []string {
	var result []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil {
				result = append(result, filepath.ToSlash(rel))
			}
		}
	}
	return result
}

func buildDependencyUpdatesCommand(action pipeline.DependencyUpdatesAction) (string, error) {
	cfg := normalizeDependencyUpdatesConfig(action)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(cfgJSON)
	return fmt.Sprintf("python3 - <<'PY'\n%s\nPY", strings.ReplaceAll(dependencyUpdatesPythonScript, "__CONFIG_B64__", encoded)), nil
}

func (s *Server) executeDependencyUpdatesAction(clawID, stageID string, action pipeline.DependencyUpdatesAction) (*pipelineRunResult, error) {
	command, err := buildDependencyUpdatesCommand(action)
	if err != nil {
		return nil, err
	}
	timeout, err := dependencyUpdatesTimeout(action)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	result, err := s.executePipelineCommand(clawID, command, timeout)
	if result != nil {
		result.Command, result.StartedAt, result.DurationMs = command, started, time.Since(started).Milliseconds()
		s.persistPipelineOutput(clawID, stageID, dependencyUpdatesOutputName(action), result)
	}
	return result, err
}

// formatDependencyUpdateFailure extracts a concise, actionable failure reason
// from the dependency update command output. The dependency update script emits
// JSON; when it fails, surfacing the specific command(s) that failed is far more
// useful than a raw blob of stdout/stderr that may be dominated by nix shellHook
// banners or toolchain download noise.
func formatDependencyUpdateFailure(result *pipelineRunResult) string {
	if result == nil {
		return "Dependency update step failed"
	}
	combined := strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
	if combined == "" {
		return "Dependency update step failed (no output)"
	}

	var output struct {
		Commands []struct {
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
			ExitCode int    `json:"exit_code"`
			Stderr   string `json:"stderr"`
			Stdout   string `json:"stdout"`
			Failed   *bool  `json:"failed,omitempty"`
		} `json:"commands"`
	}
	// The script may print non-JSON (e.g. shellHook banners) before the JSON.
	// Try to parse the last JSON object in the output, which is the result document.
	// The object is not guaranteed to be on a single line, so scan by matching
	// braces rather than splitting on newlines.
	if doc := lastJSONObject(combined); doc != nil {
		_ = json.Unmarshal(doc, &output)
	}

	var parts []string
	for _, cmd := range output.Commands {
		isFailed := cmd.ExitCode != 0
		if cmd.Failed != nil {
			isFailed = *cmd.Failed
		}
		if !isFailed {
			continue
		}
		detail := cmd.Stderr
		if detail == "" {
			detail = cmd.Stdout
		}
		detail = strings.TrimSpace(detail)
		if detail == "" {
			detail = "exit code " + fmt.Sprintf("%d", cmd.ExitCode)
		} else if len(detail) > 500 {
			detail = detail[:500] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s (cwd=%s): %s", cmd.Command, cmd.Cwd, detail))
	}
	if len(parts) > 0 {
		return "Dependency update step failed: " + strings.Join(parts, "; ")
	}
	// Fallback to raw output if we couldn't parse a structured reason.
	if len(combined) > 2000 {
		return "Dependency update step failed: " + combined[:2000] + "..."
	}
	return "Dependency update step failed: " + combined
}

// formatDependencyUpdateSummary returns a concise, human-readable summary of a
// successful dependency update run based on the JSON result document emitted by
// the dependency update script. It is surfaced in the web UI so users can see
// what the step did without reading the full structured output.
func formatDependencyUpdateSummary(result *pipelineRunResult) string {
	if result == nil {
		return "Dependency updates completed"
	}
	combined := strings.TrimSpace(result.Stdout)
	if combined == "" {
		return "Dependency updates completed"
	}

	var output struct {
		Ecosystems []string `json:"ecosystems"`
		Manifests  []struct {
			Ecosystem string `json:"ecosystem"`
			Path      string `json:"path"`
		} `json:"manifests"`
		Updates []struct {
			Applied bool `json:"applied"`
		} `json:"updates"`
		FilesChanged []string `json:"files_changed"`
	}
	if doc := lastJSONObject(combined); doc != nil {
		_ = json.Unmarshal(doc, &output)
	}

	applied := 0
	skipped := 0
	for _, update := range output.Updates {
		if update.Applied {
			applied++
		} else {
			skipped++
		}
	}

	parts := []string{"Dependency updates completed"}
	if len(output.Ecosystems) > 0 {
		parts = append(parts, fmt.Sprintf("%d ecosystem(s)", len(output.Ecosystems)))
	}
	if len(output.Manifests) > 0 {
		parts = append(parts, fmt.Sprintf("%d manifest(s)", len(output.Manifests)))
	}
	if applied > 0 || skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d applied, %d skipped", applied, skipped))
	}
	if len(output.FilesChanged) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) changed", len(output.FilesChanged)))
	}
	return strings.Join(parts, ": ")
}

// lastJSONObject returns the last valid JSON object in s, or nil if none is found.
// It is used to extract the dependency update result document from output that may
// contain non-JSON banners or log lines before or after the JSON.
func lastJSONObject(s string) []byte {
	for close := strings.LastIndex(s, "}"); close >= 0; close = strings.LastIndex(s[:close], "}") {
		for open := strings.LastIndex(s[:close], "{"); open >= 0; open = strings.LastIndex(s[:open], "{") {
			candidate := s[open : close+1]
			var v any
			if json.Unmarshal([]byte(candidate), &v) == nil {
				return []byte(candidate)
			}
		}
	}
	return nil
}

const dependencyUpdatesPythonScript = `import base64
import fnmatch
import json
import os
import pathlib
import re
import shlex
import shutil
import subprocess
import sys

CONFIG = json.loads(base64.b64decode("__CONFIG_B64__").decode("utf-8"))
ROOT = pathlib.Path.cwd()
SKIP_DIRS = {".git", "node_modules", "vendor", ".next", "dist", "build"}

result = {
    "ecosystems": [],
    "manifests": [],
    "updates": [],
    "commands": [],
    "files_changed": [],
}
had_failure = False

def enabled_ecosystems():
    values = [str(v).strip().lower() for v in CONFIG.get("ecosystems") or ["auto"] if str(v).strip()]
    if not values or "auto" in values:
        return {"go", "npm"}
    return set(values)

ECOSYSTEMS = enabled_ecosystems()
result["ecosystems"] = sorted(ECOSYSTEMS)
print(f"Starting dependency updates for ecosystem(s): {', '.join(result['ecosystems'])}", file=sys.stderr)

def rel(path):
    return path.relative_to(ROOT).as_posix()

def should_skip(path):
    return any(part in SKIP_DIRS for part in path.parts)

def is_excluded_path(path):
    rel = str(path)
    patterns = CONFIG.get("exclude_paths") or []
    for p in patterns:
        p = str(p).strip()
        if not p:
            continue
        if fnmatch.fnmatch(rel, p):
            return True
        if "*" not in p and "?" not in p:
            if rel == p or rel.startswith(p + "/"):
                return True
    return False

def allowed(name):
    allow = CONFIG.get("allow") or ["*"]
    ignore = CONFIG.get("ignore") or []
    return any(fnmatch.fnmatch(name, p) for p in allow) and not any(fnmatch.fnmatch(name, p) for p in ignore)

def update_record(ecosystem, name, from_version, to_version, kind, applied, group="default", skipped_reason=None):
    record = {
        "ecosystem": ecosystem,
        "name": name,
        "from": from_version,
        "to": to_version,
        "type": kind,
        "applied": applied,
        "group": group,
    }
    if skipped_reason:
        record["skipped_reason"] = skipped_reason
    result["updates"].append(record)
    if applied:
        print(f"Applying {ecosystem} update: {name} {from_version} -> {to_version}", file=sys.stderr)
    else:
        print(f"Skipping {ecosystem} update: {name} ({skipped_reason})", file=sys.stderr)

def version_parts(version):
    version = str(version or "").strip().lstrip("v")
    match = re.search(r"(\d+)(?:\.(\d+))?(?:\.(\d+))?", version)
    if not match:
        return None
    return tuple(int(p or 0) for p in match.groups())

def update_type(old, new):
    old_parts = version_parts(old)
    new_parts = version_parts(new)
    if not old_parts or not new_parts:
        return "unknown"
    if new_parts[0] > old_parts[0]:
        return "major"
    if new_parts[1] > old_parts[1]:
        return "minor"
    if new_parts[2] > old_parts[2]:
        return "patch"
    return "unknown"

def run_command(cwd, command, allowed_exit_codes=(0,), record_failure=True):
    global had_failure
    proc = subprocess.run(command, cwd=str(cwd), shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    failed = proc.returncode not in allowed_exit_codes and record_failure
    if failed:
        had_failure = True
    result["commands"].append({
        "command": command,
        "cwd": rel(cwd),
        "exit_code": proc.returncode,
        "stdout": proc.stdout[-8000:],
        "stderr": proc.stderr[-8000:],
        "failed": failed,
    })
    return proc

def run_command_with_retry(cwd, command, allowed_exit_codes=(0,), max_attempts=2):
    for attempt in range(1, max_attempts + 1):
        proc = run_command(cwd, command, allowed_exit_codes, record_failure=(attempt == max_attempts))
        if proc.returncode in allowed_exit_codes:
            return proc
    return proc

def discover():
    manifests = []
    paths = CONFIG.get("paths") or ["."]
    for configured in paths:
        base = (ROOT / configured).resolve()
        try:
            base.relative_to(ROOT)
        except ValueError:
            continue
        if not base.exists():
            continue
        if base.is_file():
            base = base.parent
        if is_excluded_path(rel(base)):
            continue
        for current, dirs, files in os.walk(base):
            current_path = pathlib.Path(current)
            rel_current = rel(current_path)
            if is_excluded_path(rel_current):
                dirs[:] = []
                continue
            dirs[:] = [d for d in dirs if d not in SKIP_DIRS and not is_excluded_path(rel_current + "/" + d)]
            if should_skip(current_path.relative_to(ROOT)):
                continue
            file_set = set(files)
            if "go" in ECOSYSTEMS and "go.mod" in file_set:
                mod_path = rel(current_path / "go.mod")
                if not is_excluded_path(mod_path):
                    locks = []
                    if "go.sum" in file_set:
                        locks.append(rel(current_path / "go.sum"))
                    manifests.append({"ecosystem": "go", "path": mod_path, "lockfiles": locks})
            if "npm" in ECOSYSTEMS and "package.json" in file_set:
                pkg_path = rel(current_path / "package.json")
                if not is_excluded_path(pkg_path):
                    locks = []
                    for lock in ("package-lock.json", "npm-shrinkwrap.json"):
                        if lock in file_set:
                            locks.append(rel(current_path / lock))
                    manifests.append({"ecosystem": "npm", "path": pkg_path, "lockfiles": locks})
    manifests.sort(key=lambda item: (item["ecosystem"], item["path"]))
    return manifests

def file_snapshot(files):
    snap = {}
    for file in files:
        path = ROOT / file
        if path.exists():
            snap[file] = path.read_bytes()
        else:
            snap[file] = None
    return snap

def changed_files(before):
    changed = []
    for file, old in before.items():
        path = ROOT / file
        new = path.read_bytes() if path.exists() else None
        if new != old:
            changed.append(file)
    return changed

def parse_go_modules(raw):
    decoder = json.JSONDecoder()
    idx = 0
    modules = []
    raw = raw.strip()
    while idx < len(raw):
        obj, next_idx = decoder.raw_decode(raw, idx)
        modules.append(obj)
        idx = next_idx
        while idx < len(raw) and raw[idx].isspace():
            idx += 1
    return modules

def collect_files(manifests):
    files = []
    for manifest in manifests:
        files.append(manifest["path"])
        files.extend(manifest.get("lockfiles") or [])
    return sorted(set(files))

manifests = discover()
result["manifests"] = manifests
print(f"Discovered {len(manifests)} manifest(s)", file=sys.stderr)
before = file_snapshot(collect_files(manifests))

for manifest in manifests:
    print(f"Processing {manifest['ecosystem']} manifest {manifest['path']}", file=sys.stderr)
    manifest_path = ROOT / manifest["path"]
    cwd = manifest_path.parent

    if manifest["ecosystem"] == "go":
        if shutil.which("go") is None:
            result["commands"].append({"command": "go", "cwd": rel(cwd), "exit_code": 127, "stderr": "go executable not found"})
            had_failure = True
            continue
        failure_before = had_failure
        listed = run_command(cwd, "go list -e -m -u -json all")
        # Use -e so go list keeps going when a module behind a replace directive or an
        # unreplaceable placeholder version cannot be resolved. Modules with errors are
        # emitted as JSON objects with an Error field; we skip them below. If an earlier
        # module update left this module's go.mod stale, go list will refuse to run until
        # it is tidied; that is a recoverable failure. Even if go list exits non-zero,
        # stdout may still contain valid JSON for usable modules, so we try to parse it
        # and apply whatever we can.
        if listed.returncode != 0 and "updates to go.mod needed" in listed.stderr:
            if not failure_before:
                had_failure = False
            run_command(cwd, "go mod tidy", record_failure=False)
            listed = run_command(cwd, "go list -e -m -u -json all")
        try:
            apply_updates = []
            for module in parse_go_modules(listed.stdout):
                name = module.get("Path", "")
                # Skip the main module, modules pinned by replace directives, modules
                # that carry a resolution error, and transitive-only dependencies. Only
                # direct dependencies can be safely updated by go get; replaced modules
                # would conflict with the directive and error-carrying modules cannot be
                # trusted to provide a valid update target.
                if module.get("Main") or module.get("Replace") or module.get("Error") or module.get("Indirect"):
                    continue
                update = module.get("Update") or {}
                if not update:
                    continue
                from_version = module.get("Version", "")
                to_version = update.get("Version", "")
                # Skip placeholder/invalid versions that appear behind replace directives
                # or in broken module graphs (e.g. v0.0.0).
                if to_version.startswith("v0.0.0"):
                    update_record("go", name, from_version, to_version, "unknown", False, skipped_reason="invalid/placeholder version")
                    continue
                kind = update_type(from_version, to_version)
                if not allowed(name):
                    update_record("go", name, from_version, to_version, kind, False, skipped_reason="filtered by allow/ignore")
                    continue
                if kind == "major" and not CONFIG.get("include_major"):
                    update_record("go", name, from_version, to_version, kind, False, skipped_reason="major updates disabled")
                    continue
                update_record("go", name, from_version, to_version, kind, True)
                apply_updates.append(f"{name}@{to_version}")
            # Apply each Go update individually. Large batched commands can hit
            # Go-internal go.mod races and version conflicts between unrelated
            # modules. Smaller commands isolate failures and let successful updates
            # remain applied; a single retry handles transient write-conflicts.
            for update in apply_updates:
                run_command_with_retry(cwd, "go get " + shlex.quote(update))
            # Always tidy after applying updates so that partial failures still leave
            # go.mod/go.sum in a consistent state.
            if apply_updates:
                run_command(cwd, "go mod tidy")
        except Exception as exc:
            result["commands"].append({"command": "parse go list output", "cwd": rel(cwd), "exit_code": 1, "stderr": str(exc)})
            had_failure = True
    elif manifest["ecosystem"] == "npm":
        if shutil.which("npm") is None:
            result["commands"].append({"command": "npm", "cwd": rel(cwd), "exit_code": 127, "stderr": "npm executable not found"})
            had_failure = True
            continue
        outdated = run_command(cwd, "npm outdated --json", allowed_exit_codes=(0, 1))
        if outdated.stdout.strip():
            try:
                apply_updates = []
                data = json.loads(outdated.stdout)
                for name, info in sorted(data.items()):
                    current = str(info.get("current", ""))
                    wanted = str(info.get("wanted", ""))
                    latest = str(info.get("latest", ""))
                    kind = update_type(current, wanted)
                    if not allowed(name):
                        update_record("npm", name, current, wanted, kind, False, skipped_reason="filtered by allow/ignore")
                        continue
                    if kind == "major" and not CONFIG.get("include_major"):
                        update_record("npm", name, current, wanted, kind, False, skipped_reason="major updates disabled")
                    else:
                        update_record("npm", name, current, wanted, kind, True)
                        apply_updates.append(name)
                    if latest and latest != wanted and update_type(current, latest) == "major" and not CONFIG.get("include_major"):
                        update_record("npm", name, current, latest, "major", False, group="major", skipped_reason="major updates disabled")
                if apply_updates:
                    run_command(cwd, "npm update --package-lock-only " + " ".join(shlex.quote(value) for value in apply_updates))
            except Exception as exc:
                result["commands"].append({"command": "parse npm outdated output", "cwd": rel(cwd), "exit_code": 1, "stderr": str(exc)})
                had_failure = True

result["files_changed"] = changed_files(before)
print(f"Dependency update complete: {len(result['updates'])} update(s), {len(result['files_changed'])} file(s) changed", file=sys.stderr)
print(json.dumps(result, sort_keys=True))
sys.exit(1 if had_failure else 0)
`
