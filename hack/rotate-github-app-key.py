#!/usr/bin/env python3
"""Rotate the GitHub App private key used by an ElasticClaw hub.

The key lives in two places that must stay in sync:

  1. hub.yaml -> github[] (hub-level app, feeds the git credential helper)
  2. workspaces/<ws>/.elasticclaw-managed/github_apps.yaml (workspace app)

Both are updated through the hub API, so no restart is needed: settings are
written to disk and swapped into the running config, and token providers are
built per use from the current config.

Usage:
    paste the new key into hack/github-app-key.pem (gitignored), then
    hack/rotate-github-app-key.py --check-only   # validate, write nothing
    hack/rotate-github-app-key.py                # rotate hub + workspace

Reads the hub URL and admin token from ~/.elasticclaw/config.yaml (profile
"faster" by default); override with --url / --token or ELASTICCLAW_HUB_URL /
ELASTICCLAW_HUB_TOKEN.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

CONFIG_PATH = os.path.expanduser("~/.elasticclaw/config.yaml")
DEFAULT_PEM = os.path.join(os.path.dirname(os.path.abspath(__file__)), "github-app-key.pem")
MANAGED_DIR = ".elasticclaw-managed"


def die(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(1)


# --- local profile ---------------------------------------------------------


def read_profile(profile: str) -> dict:
    """Parse the flat profiles block of ~/.elasticclaw/config.yaml."""
    if not os.path.exists(CONFIG_PATH):
        return {}
    fields: dict[str, str] = {}
    in_profiles = False
    current = None
    for line in open(CONFIG_PATH, encoding="utf-8"):
        line = line.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if re.match(r"^\S", line):
            in_profiles = line.startswith("profiles:")
            current = None
            continue
        if not in_profiles:
            continue
        m = re.match(r"^ {4}(\S+):\s*$", line)
        if m:
            current = m.group(1)
            continue
        m = re.match(r"^ {8}(\S+):\s*(.*)$", line)
        if m and current == profile:
            fields[m.group(1)] = m.group(2).strip().strip('"').strip("'")
    return fields


# --- hub API ---------------------------------------------------------------


class Hub:
    def __init__(self, url: str, token: str):
        self.url = url.rstrip("/")
        self.token = token

    def call(self, method: str, path: str, body: dict | None = None) -> dict:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.url + path, data=data, method=method)
        req.add_header("Authorization", "Bearer " + self.token)
        if data:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                raw = resp.read().decode()
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace").strip()
            die(f"{method} {path} -> HTTP {e.code}: {detail[:300]}")
        except urllib.error.URLError as e:
            die(f"{method} {path} -> {e.reason}")
        return json.loads(raw) if raw.strip() else {}


# --- pem -------------------------------------------------------------------


def load_pem(path: str) -> str:
    if not os.path.exists(path):
        die(f"PEM file not found: {path} (copy hack/github-app-key.pem.example and paste the key)")
    pem = open(path, encoding="utf-8").read().strip()
    if "<paste" in pem:
        die(f"{path} still has the placeholder — paste the real key into it")
    if "PRIVATE KEY" not in pem:
        die(f"{path} does not look like a private key PEM")
    if subprocess.run(["which", "openssl"], capture_output=True).returncode == 0:
        check = subprocess.run(
            ["openssl", "rsa", "-in", path, "-noout", "-check"],
            capture_output=True,
            text=True,
        )
        if check.returncode != 0:
            die(f"openssl rejected the key: {check.stderr.strip()[:200]}")
        print("  openssl: RSA key OK")
    return pem


# --- backup ----------------------------------------------------------------


def backup_remote(profile: dict, workspace: str) -> None:
    ssh_uri, ssh_key = profile.get("ssh_uri"), profile.get("ssh_key")
    if not ssh_uri or not ssh_key:
        print("  skipped: profile has no ssh_uri/ssh_key (use --skip-backup to silence)")
        return
    host = re.sub(r"^ssh://", "", ssh_uri).split(":")[0]
    stamp = time.strftime("%Y%m%d%H%M%S")
    remote = (
        "set -e; "
        f"sudo cp -a /root/.elasticclaw/hub.yaml /root/.elasticclaw/hub.yaml.pre-token-update.{stamp}; "
        f"sudo cp -a /root/.elasticclaw/workspaces/{workspace}/{MANAGED_DIR}/github_apps.yaml "
        f"/root/.elasticclaw/workspaces/{workspace}/{MANAGED_DIR}/github_apps.yaml.pre-token-update.{stamp}; "
        f"echo backed up .pre-token-update.{stamp}"
    )
    out = subprocess.run(
        [
            "ssh", "-i", os.path.expanduser(ssh_key),
            "-o", "IdentitiesOnly=yes",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "ConnectTimeout=25",
            host, remote,
        ],
        capture_output=True,
        text=True,
    )
    if out.returncode != 0:
        die(f"backup failed (nothing was changed): {out.stderr.strip()[:300]}")
    print("  " + out.stdout.strip())


# --- main ------------------------------------------------------------------


def main() -> None:
    ap = argparse.ArgumentParser(description="Rotate the hub + workspace GitHub App private key")
    ap.add_argument(
        "--pem",
        default=DEFAULT_PEM,
        help="path to the new GitHub App private key (default: hack/github-app-key.pem)",
    )
    ap.add_argument("--profile", default="faster", help="local CLI profile (default: faster)")
    ap.add_argument("--workspace", default="faster", help="hub workspace (default: faster)")
    ap.add_argument("--app-id", type=int, default=0, help="app id to rotate (default: the only configured one)")
    ap.add_argument("--url", default=os.environ.get("ELASTICCLAW_HUB_URL", ""))
    ap.add_argument("--token", default=os.environ.get("ELASTICCLAW_HUB_TOKEN", ""))
    ap.add_argument("--check-only", action="store_true", help="validate the new key against GitHub, write nothing")
    ap.add_argument("--skip-backup", action="store_true", help="do not snapshot the remote config files first")
    ap.add_argument("--yes", action="store_true", help="do not prompt before writing")
    args = ap.parse_args()

    profile = read_profile(args.profile)
    url = args.url or profile.get("url", "")
    token = args.token or profile.get("token", "")
    if not url or not token:
        die(f"no url/token for profile {args.profile!r}; pass --url/--token")
    hub = Hub(url, token)

    print(f"hub: {url}  workspace: {args.workspace}")

    print("1/6 reading the new key")
    pem = load_pem(args.pem)

    print("2/6 reading current config")
    settings = hub.call("GET", "/api/settings")
    hub_apps = settings.get("github") or []
    if not hub_apps:
        die("hub has no GitHub App configured — nothing to rotate")
    app_id = args.app_id or hub_apps[0]["appId"]
    if args.app_id and not any(a["appId"] == app_id for a in hub_apps):
        die(f"app id {app_id} is not configured on this hub")
    ws_apps = (hub.call("GET", f"/api/workspaces/{args.workspace}/github-apps") or {}).get("githubApps") or []
    ws_targets = [a for a in ws_apps if a.get("appId") == app_id]
    print(f"  app id {app_id}: 1 hub entry, {len(ws_targets)} workspace entr{'y' if len(ws_targets) == 1 else 'ies'}")
    if not ws_targets:
        print(f"  warning: workspace {args.workspace} has no entry for this app; only hub.yaml will change")

    print("3/6 testing the new key against GitHub (no write)")
    view = hub.call("POST", "/api/settings/github/test", {"appId": app_id, "privateKeyPem": pem})
    if view.get("permCheckError"):
        die(f"new key rejected: {view['permCheckError']}")
    missing = [p for p in view.get("permissions", []) if not p.get("ok")]
    for p in view.get("permissions", []):
        print(f"  {'ok ' if p['ok'] else 'MISSING'} {p['name']}: granted={p['granted'] or '-'} needed={p['needed']}")
    if missing:
        die("the app installation is missing required permissions — fix them on GitHub first")

    if args.check_only:
        print("check-only: key is valid and has the required permissions; nothing written")
        return

    if not args.yes:
        answer = input(f"write this key to hub.yaml and workspace {args.workspace}? [y/N] ").strip().lower()
        if answer not in ("y", "yes"):
            die("aborted by user")

    print("4/6 backing up remote config")
    if args.skip_backup:
        print("  skipped (--skip-backup)")
    else:
        backup_remote(profile, args.workspace)

    print("5/6 writing")
    # PATCH replaces the whole github list: resend every app, new PEM only for
    # the target; an empty privateKeyPem preserves the stored key server-side.
    patch = [
        {"appId": a["appId"], "url": a.get("url", ""), "privateKeyPem": pem if a["appId"] == app_id else ""}
        for a in hub_apps
    ]
    hub.call("PATCH", "/api/settings", {"github": patch})
    print("  hub.yaml updated")
    for a in ws_targets:
        hub.call(
            "PUT",
            f"/api/workspaces/{args.workspace}/github-apps",
            {
                "name": a["name"],
                "appId": a["appId"],
                "url": a.get("url", ""),
                "installation": a.get("installation", ""),
                "privateKeyPem": pem,
            },
        )
        print(f"  workspace app {a['name']!r} updated")

    print("6/6 verifying")
    after = hub.call("GET", "/api/settings")
    app_after = next((a for a in after.get("github") or [] if a["appId"] == app_id), None)
    if not app_after or not app_after.get("keySet"):
        die("hub reports no key set after the write — restore the backup")
    if app_after.get("permCheckError"):
        die(f"hub cannot use the new key: {app_after['permCheckError']} — restore the backup")
    bad = [p["name"] for p in app_after.get("permissions", []) if not p.get("ok")]
    if bad:
        die(f"permissions broken after write: {', '.join(bad)} — restore the backup")
    print("  hub: key set, permissions ok")
    for a in (hub.call("GET", f"/api/workspaces/{args.workspace}/github-apps") or {}).get("githubApps") or []:
        if a.get("appId") != app_id:
            continue
        if not a.get("private_key_set"):
            die(f"workspace app {a['name']!r} has no key after the write — restore the backup")
        installs = ", ".join(a.get("installations") or []) or "none resolved"
        print(f"  workspace {a['name']!r}: key set, installations: {installs}")

    print(
        "\ndone. New claws pick the key up immediately; already-running sandboxes "
        "keep their current installation token until it expires (<= 1h)."
    )


if __name__ == "__main__":
    main()
