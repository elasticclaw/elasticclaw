package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
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

func dependencyUpdatesConfigured(action pipeline.DependencyUpdatesAction) bool {
	return action.Enabled ||
		len(action.Ecosystems) > 0 ||
		len(action.Paths) > 0 ||
		strings.TrimSpace(action.Grouping) != "" ||
		action.IncludeMajor ||
		action.SeparateMajor != nil ||
		action.SeparateSecurity != nil ||
		action.SeparateRuntime != nil ||
		len(action.Allow) > 0 ||
		len(action.Ignore) > 0 ||
		strings.TrimSpace(action.Output) != "" ||
		strings.TrimSpace(action.Timeout) != "" ||
		action.ContinueOnError
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
			if d.IsDir() && shouldSkipDependencyUpdateDir(d.Name()) {
				if path == base {
					return nil
				}
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			dir := filepath.Dir(path)
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relPath = filepath.ToSlash(relPath)
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
	command, err := s.dependencyUpdatesCommandForClaw(clawID, action)
	if err != nil {
		return nil, err
	}
	timeout, err := dependencyUpdatesTimeout(action)
	if err != nil {
		return nil, err
	}
	result, err := s.executePipelineCommand(clawID, command, timeout)
	if result != nil {
		s.persistPipelineOutput(clawID, stageID, dependencyUpdatesOutputName(action), result)
	}
	return result, err
}

func (s *Server) dependencyUpdatesCommandForClaw(clawID string, action pipeline.DependencyUpdatesAction) (string, error) {
	command, err := buildDependencyUpdatesCommand(action)
	if err != nil {
		return "", err
	}
	useNix, err := s.clawUsesNix(clawID)
	if err != nil {
		return "", err
	}
	return wrapDependencyUpdatesCommand(command, useNix), nil
}

func (s *Server) clawUsesNix(clawID string) (bool, error) {
	nixEnabled, err := s.st().Claws().NixEnabled(s.base(), clawID)
	if err != nil {
		return false, fmt.Errorf("load agent nix setting: %w", err)
	}
	return nixEnabled, nil
}

func wrapDependencyUpdatesCommand(command string, useNix bool) string {
	if !useNix {
		return command
	}
	return "nix develop --accept-flake-config -c bash -lc " + shellQuote(command)
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

def rel(path):
    return path.relative_to(ROOT).as_posix()

def should_skip(path):
    return any(part in SKIP_DIRS for part in path.parts)

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

def run_command(cwd, command, allowed_exit_codes=(0,)):
    global had_failure
    proc = subprocess.run(command, cwd=str(cwd), shell=True, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    result["commands"].append({
        "command": command,
        "cwd": rel(cwd),
        "exit_code": proc.returncode,
        "stdout": proc.stdout[-4000:],
        "stderr": proc.stderr[-4000:],
    })
    if proc.returncode not in allowed_exit_codes:
        had_failure = True
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
        for current, dirs, files in os.walk(base):
            current_path = pathlib.Path(current)
            dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
            if should_skip(current_path.relative_to(ROOT)):
                continue
            file_set = set(files)
            if "go" in ECOSYSTEMS and "go.mod" in file_set:
                locks = []
                if "go.sum" in file_set:
                    locks.append(rel(current_path / "go.sum"))
                manifests.append({"ecosystem": "go", "path": rel(current_path / "go.mod"), "lockfiles": locks})
            if "npm" in ECOSYSTEMS and "package.json" in file_set:
                locks = []
                for lock in ("package-lock.json", "npm-shrinkwrap.json"):
                    if lock in file_set:
                        locks.append(rel(current_path / lock))
                manifests.append({"ecosystem": "npm", "path": rel(current_path / "package.json"), "lockfiles": locks})
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
before = file_snapshot(collect_files(manifests))

for manifest in manifests:
    manifest_path = ROOT / manifest["path"]
    cwd = manifest_path.parent
    if manifest["ecosystem"] == "go":
        if shutil.which("go") is None:
            result["commands"].append({"command": "go", "cwd": rel(cwd), "exit_code": 127, "stderr": "go executable not found"})
            had_failure = True
            continue
        listed = run_command(cwd, "go list -m -u -json all")
        if listed.returncode == 0:
            try:
                apply_updates = []
                for module in parse_go_modules(listed.stdout):
                    update = module.get("Update") or {}
                    name = module.get("Path", "")
                    if not update:
                        continue
                    from_version = module.get("Version", "")
                    to_version = update.get("Version", "")
                    kind = update_type(from_version, to_version)
                    if not allowed(name):
                        update_record("go", name, from_version, to_version, kind, False, skipped_reason="filtered by allow/ignore")
                        continue
                    if kind == "major" and not CONFIG.get("include_major"):
                        update_record("go", name, from_version, to_version, kind, False, skipped_reason="major updates disabled")
                        continue
                    update_record("go", name, from_version, to_version, kind, True)
                    apply_updates.append(f"{name}@{to_version}")
                if apply_updates:
                    run_command(cwd, "go get " + " ".join(shlex.quote(value) for value in apply_updates))
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
print(json.dumps(result, sort_keys=True))
sys.exit(1 if had_failure else 0)
`
