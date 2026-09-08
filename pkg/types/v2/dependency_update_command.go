package v2

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// BuildDependencyUpdateCommand returns the shell command that runs a v2
// dependency.update effect. It is shared between the hub (for validation /
// materialization) and the bridge (for execution), so the command produced for
// a given config is identical regardless of where it is invoked.
func BuildDependencyUpdateCommand(cfg DependencyUpdateConfig) (string, error) {
	cfg = normalizeDependencyUpdateConfig(cfg)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(cfgJSON)
	return fmt.Sprintf("python3 - <<'PY'\n%s\nPY", strings.ReplaceAll(dependencyUpdatesPythonScript, "__CONFIG_B64__", encoded)), nil
}

func normalizeDependencyUpdateConfig(cfg DependencyUpdateConfig) DependencyUpdateConfig {
	out := DependencyUpdateConfig{
		Ecosystems:       cleanStringList(cfg.Ecosystems),
		Paths:            cleanStringList(cfg.Paths),
		ExcludePaths:     cleanStringList(cfg.ExcludePaths),
		Grouping:         strings.TrimSpace(cfg.Grouping),
		IncludeMajor:     cfg.IncludeMajor,
		SeparateMajor:    cfg.SeparateMajor,
		SeparateSecurity: cfg.SeparateSecurity,
		SeparateRuntime:  cfg.SeparateRuntime,
		Allow:            cleanStringList(cfg.Allow),
		Ignore:           cleanStringList(cfg.Ignore),
	}
	if out.SeparateMajor == nil {
		v := true
		out.SeparateMajor = &v
	}
	if out.SeparateSecurity == nil {
		v := true
		out.SeparateSecurity = &v
	}
	if out.SeparateRuntime == nil {
		v := true
		out.SeparateRuntime = &v
	}
	if len(out.Ecosystems) == 0 {
		out.Ecosystems = []string{"auto"}
	}
	if len(out.Paths) == 0 {
		out.Paths = []string{"."}
	}
	if out.Grouping == "" {
		out.Grouping = "all"
	}
	if len(out.Allow) == 0 {
		out.Allow = []string{"*"}
	}
	return out
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
        if listed.returncode != 0 and "updates to go.mod needed" in listed.stderr:
            if not failure_before:
                had_failure = False
            run_command(cwd, "go mod tidy", record_failure=False)
            listed = run_command(cwd, "go list -e -m -u -json all")
        try:
            apply_updates = []
            for module in parse_go_modules(listed.stdout):
                name = module.get("Path", "")
                if module.get("Main") or module.get("Replace") or module.get("Error") or module.get("Indirect"):
                    continue
                update = module.get("Update") or {}
                if not update:
                    continue
                from_version = module.get("Version", "")
                to_version = update.get("Version", "")
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
            for update in apply_updates:
                run_command_with_retry(cwd, "go get " + shlex.quote(update))
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
