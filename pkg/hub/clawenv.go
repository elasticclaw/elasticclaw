// Environment and shell-script builders injected into claw sandboxes.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// resolveLinearToken finds the Linear API token for the given workspace label.
// If workspace is empty or not found, returns the first token if only one is configured.
func resolveLinearToken(cfg *types.HubConfig, workspace string) string {
	if len(cfg.Linear) == 0 {
		return ""
	}
	for _, l := range cfg.Linear {
		if workspace != "" && l.Workspace == workspace {
			return l.Token
		}
	}
	// Default: first entry (when workspace is empty or no match)
	return cfg.Linear[0].Token
}

// buildLinearEnv returns a shell export line for LINEAR_API_KEY if a token is set.
func buildLinearEnv(token string) string {
	if token == "" {
		return "# Linear not configured"
	}
	return fmt.Sprintf("export LINEAR_API_KEY=%q", token)
}

// buildLLMKeyEnv converts llm_keys slice to shell env var export lines.
// If selectedKeyName is non-empty, the selected key is prioritized over default keys.
// All keys are exported so each claw has access to whichever provider it needs.
func buildLLMKeyEnv(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	seen := map[string]bool{}

	// First pass: export the selected key if specified
	if selectedKeyName != "" {
		for _, k := range keys {
			if k.Name == selectedKeyName && llmKeyHasRequiredAPIKey(k) {
				envVar := k.EnvVarName()
				seen[envVar] = true
				fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
				break
			}
		}
	}

	// Second pass: export default keys for providers not yet seen
	for _, k := range keys {
		if !k.Default || !llmKeyHasRequiredAPIKey(k) {
			continue
		}
		envVar := k.EnvVarName()
		if seen[envVar] {
			continue
		}
		seen[envVar] = true
		fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
	}
	// Third pass: export non-default keys for providers not yet seen
	for _, k := range keys {
		if !llmKeyHasRequiredAPIKey(k) {
			continue
		}
		envVar := k.EnvVarName()
		if seen[envVar] {
			continue
		}
		seen[envVar] = true
		fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
	}
	return b.String()
}

// resolveDefaultModelForKey returns the effective model for a given LLM key.
// If the hub's default model matches the key's provider, use it; otherwise construct a provider-specific default.
func resolveDefaultModelForKey(hubCfg *types.HubConfig, key *types.LLMKeyConfig) string {
	if key == nil {
		return hubCfg.DefaultModel
	}

	// Use per-key default model if set; normalize to include provider prefix
	if key.DefaultModel != "" {
		prefix := key.Provider + "/"
		if !strings.HasPrefix(key.DefaultModel, prefix) {
			return prefix + key.DefaultModel
		}
		return key.DefaultModel
	}

	// Check if hub's DefaultModel matches this key's provider
	if hubCfg.DefaultModel != "" && strings.HasPrefix(hubCfg.DefaultModel, key.Provider+"/") {
		return hubCfg.DefaultModel
	}

	// Construct a provider-specific default model
	switch key.Provider {
	case "anthropic":
		return "anthropic/claude-sonnet-4-6"
	case "openai":
		return "openai/gpt-5.5"
	case "codex":
		return "codex/gpt-5.5"
	case "grok":
		return "grok/grok-build-0.1"
	case "fireworks":
		return defaultFireworksModel
	case "groq":
		return "groq/llama-3.3-70b-versatile"
	case "deepseek":
		return "deepseek/deepseek-chat"
	case "ollama":
		return "ollama/qwen2.5-coder:1.5b"
	case "moonshot":
		return "moonshot/moonshot-v1-8k"
	default:
		// Fall back to hub default even if provider doesn't match
		return hubCfg.DefaultModel
	}
}

func resolveModelAndLLMKey(hubCfg *types.HubConfig, selectedKeyName, defaultModel string) (string, string) {
	if hubCfg == nil {
		return defaultModel, selectedKeyName
	}
	resolvedModel := defaultModel
	resolvedKeyName := selectedKeyName
	if resolvedModel == "" {
		activeKey := resolveActiveKey(hubCfg.LLMKeys, selectedKeyName)
		if activeKey != nil {
			if resolvedKeyName == "" || resolvedKeyName != activeKey.Name {
				resolvedKeyName = activeKey.Name
			}
			resolvedModel = resolveDefaultModelForKey(hubCfg, activeKey)
		}
	}
	if resolvedModel == "" {
		resolvedModel = hubCfg.DefaultModel
	}
	return resolvedModel, resolvedKeyName
}

// buildGitHubCloneScript returns shell lines that clone repos into the current directory.
func buildGitHubCloneScript(repos []types.GitHubRepoAccess) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range repos {
		parts := strings.SplitN(r.Repo, "/", 2)
		repoName := r.Repo
		if len(parts) == 2 {
			repoName = parts[1]
		}
		fmt.Fprintf(&b, "if [ ! -d %q ]; then git clone https://github.com/%s %s && echo 'Cloned %s' || FAILED=1; else git -C %s pull --ff-only && echo 'Updated %s' || FAILED=1; fi\n",
			repoName, r.Repo, repoName, r.Repo, repoName, r.Repo)
	}
	return b.String()
}

func buildGitHubTokenProfileScript() string {
	return `# ElasticClaw GitHub App token refresh for gh.
# This intentionally resolves through the credential helper instead of storing
# the short-lived installation token generated during bootstrap.
if [ -x /usr/local/bin/elasticclaw-git-credentials ]; then
  token="$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | sed -n 's/^password=//p' | head -n1)"
  if [ -n "$token" ]; then
    export GH_TOKEN="$token"
  else
    unset GH_TOKEN
  fi
  unset token
fi
`
}

func buildGitHubTokenProfileInstallScript() string {
	return `sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null << 'PROFILEEOF'
` + buildGitHubTokenProfileScript() + `PROFILEEOF
sudo chmod +x /etc/profile.d/elasticclaw-github.sh
[ -s /etc/profile.d/elasticclaw-github.sh ] || exit 1`
}

func buildGitHubCLIWrapperInstallScript() string {
	return `if command -v gh >/dev/null 2>&1; then
  REAL_GH="$(command -v gh)"
  if [ "$REAL_GH" = "/usr/local/bin/gh" ]; then
    if grep -q "ElasticClaw GitHub App token refresh wrapper" /usr/local/bin/gh 2>/dev/null; then
      echo "GitHub gh wrapper already configured"
      REAL_GH=""
    elif [ -x /usr/local/bin/gh.elasticclaw-real ]; then
      REAL_GH="/usr/local/bin/gh.elasticclaw-real"
    else
      sudo mv /usr/local/bin/gh /usr/local/bin/gh.elasticclaw-real
      REAL_GH="/usr/local/bin/gh.elasticclaw-real"
    fi
  fi
  if [ -n "$REAL_GH" ] && [ -x "$REAL_GH" ]; then
    sudo tee /usr/local/bin/gh > /dev/null << 'GHEOF'
#!/bin/bash
# ElasticClaw GitHub App token refresh wrapper.
set +x
REAL_GH="__ELASTICCLAW_REAL_GH__"
if [ -x /usr/local/bin/elasticclaw-git-credentials ]; then
  token="$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | sed -n 's/^password=//p' | head -n1)"
  if [ -n "$token" ]; then
    export GH_TOKEN="$token"
  fi
  unset token
fi
exec "$REAL_GH" "$@"
GHEOF
    REAL_GH_ESCAPED="$(printf '%s' "$REAL_GH" | sed 's/[&\\|]/\\&/g')"
    sudo sed -i "s|__ELASTICCLAW_REAL_GH__|$REAL_GH_ESCAPED|g" /usr/local/bin/gh
    sudo chmod +x /usr/local/bin/gh
    echo "GitHub gh wrapper configured"
  fi
fi`
}

func buildDaytonaGitHubCloneScript(repos []types.GitHubRepoAccess) string {
	var b strings.Builder
	b.WriteString("export HOME=/home/daytona; export GIT_TERMINAL_PROMPT=0; set +x; cd ~/.openclaw/workspace; git config --global --get credential.helper >/dev/null || exit 1; set -o pipefail; ")
	for _, repo := range repos {
		repoName := repoDirectoryName(repo.Repo)
		cloneURL := "https://github.com/" + repo.Repo + ".git"
		fmt.Fprintf(&b, "echo %s; if [ ! -d %s ]; then git clone %s %s || { echo %s; exit 1; }; echo %s; else git -C %s remote set-url origin %s || true; git -C %s pull --ff-only || { echo %s; exit 1; }; echo %s; fi; ",
			shellQuote(fmt.Sprintf("[daytona] cloning %s into %s", repo.Repo, repoName)),
			shellQuote(repoName),
			shellQuote(cloneURL),
			shellQuote(repoName),
			shellQuote("[daytona] clone FAILED: "+repo.Repo),
			shellQuote("[daytona] clone OK: "+repo.Repo),
			shellQuote(repoName),
			shellQuote(cloneURL),
			shellQuote(repoName),
			shellQuote("[daytona] pull FAILED: "+repo.Repo),
			shellQuote("[daytona] pull OK: "+repo.Repo),
		)
	}
	return b.String()
}

var repoInstructionFileNames = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}

const repoInstructionsIndexName = "REPO_INSTRUCTIONS.md"

const repoEnvironmentIndexName = "REPO_ENVIRONMENT.md"

const repoInstructionsAgentsSection = `## Repository Instructions

If ` + "`REPO_INSTRUCTIONS.md`" + ` exists, read it before working inside any cloned repository. It lists repository-owned instruction files such as ` + "`AGENTS.md`" + `, ` + "`CLAUDE.md`" + `, and ` + "`GEMINI.md`" + `.`

const repoEnvironmentAgentsSection = `## Repository Environments

If ` + "`REPO_ENVIRONMENT.md`" + ` exists, read it before running commands inside cloned repositories. Repositories with ` + "`flake.nix`" + ` should run repo-local commands with that repository's own Nix development shell, for example ` + "`cd <repo> && nix develop --accept-flake-config -c <command>`" + `.`

func buildRepoInstructionDiscoveryScript(workspaceDir string, repos []types.GitHubRepoAccess) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `set -euo pipefail
WORKSPACE_DIR=%s
mkdir -p "$WORKSPACE_DIR"
cd "$WORKSPACE_DIR"

TMP="$(mktemp "$WORKSPACE_DIR/.repo-instructions.XXXXXX")"
FOUND=0
{
  printf '%%s\n\n' '# Repository Instructions'
  printf '%%s\n\n' 'ElasticClaw detected repository-owned agent instruction files. Read the relevant files before making changes in that repository.'
`, shellDoubleQuote(workspaceDir))
	for _, repo := range repos {
		repoName := repoDirectoryName(repo.Repo)
		fmt.Fprintf(&b, `  REPO_DIR=%s
  REPO_FOUND=0
  if [ -d "$REPO_DIR" ]; then
`, shellQuote(repoName))
		for _, fileName := range repoInstructionFileNames {
			repoPath := repoName + "/" + fileName
			fmt.Fprintf(&b, "    if [ -f \"$REPO_DIR/%s\" ]; then\n", fileName)
			fmt.Fprintf(&b, "      if [ \"$REPO_FOUND\" -eq 0 ]; then\n")
			fmt.Fprintf(&b, "        printf '\\n## %%s\\n\\n' %s\n", shellQuote(repoName))
			fmt.Fprintf(&b, "        REPO_FOUND=1\n")
			fmt.Fprintf(&b, "        FOUND=1\n")
			fmt.Fprintf(&b, "      fi\n")
			fmt.Fprintf(&b, "      printf -- '- `%%s`\\n' %s\n", shellQuote(repoPath))
			fmt.Fprintf(&b, "    fi\n")
		}
		b.WriteString("  fi\n")
	}
	fmt.Fprintf(&b, `} > "$TMP"
if [ "$FOUND" -eq 1 ]; then
  mv "$TMP" "$WORKSPACE_DIR/%s"
else
  rm -f "$TMP" "$WORKSPACE_DIR/%s"
fi

ENV_TMP="$(mktemp "$WORKSPACE_DIR/.repo-environment.XXXXXX")"
ENV_FOUND=0
{
  printf '%%s\n\n' '# Repository Environments'
  printf '%%s\n\n' 'ElasticClaw detected repository-local Nix flakes. Run commands for each repository with that repository flake instead of assuming one global project environment.'
  printf '%%s\n\n' 'For one command, use: cd <repo> && nix develop --accept-flake-config -c <command>'
  printf '%%s\n\n' 'For a sequence of commands in one repository, use: cd <repo> && nix develop --accept-flake-config'
`, repoInstructionsIndexName, repoInstructionsIndexName)
	for _, repo := range repos {
		repoName := repoDirectoryName(repo.Repo)
		fmt.Fprintf(&b, `  REPO_DIR=%s
  if [ -f "$REPO_DIR/flake.nix" ]; then
    ENV_FOUND=1
    printf -- '- %s: cd %s && nix develop --accept-flake-config -c <command>\n'
  fi
`, shellQuote(repoName), repoName, repoName)
	}
	fmt.Fprintf(&b, `} > "$ENV_TMP"
if [ "$ENV_FOUND" -eq 1 ]; then
  mv "$ENV_TMP" "$WORKSPACE_DIR/%s"
else
  rm -f "$ENV_TMP" "$WORKSPACE_DIR/%s"
fi

AGENTS_FILE="$WORKSPACE_DIR/AGENTS.md"
SECTION='## Repository Instructions'
if [ ! -f "$AGENTS_FILE" ]; then
  cat > "$AGENTS_FILE" << 'ELASTICCLAW_REPO_AGENTS'
%s
ELASTICCLAW_REPO_AGENTS
elif ! grep -Fqx "$SECTION" "$AGENTS_FILE"; then
  cat >> "$AGENTS_FILE" << 'ELASTICCLAW_REPO_AGENTS'

%s
ELASTICCLAW_REPO_AGENTS
fi

ENV_SECTION='## Repository Environments'
if ! grep -Fqx "$ENV_SECTION" "$AGENTS_FILE"; then
  cat >> "$AGENTS_FILE" << 'ELASTICCLAW_REPO_ENV'

%s
ELASTICCLAW_REPO_ENV
fi
`, repoEnvironmentIndexName, repoEnvironmentIndexName, repoInstructionsAgentsSection, repoInstructionsAgentsSection, repoEnvironmentAgentsSection)
	return b.String()
}

func buildBestEffortRepoInstructionDiscoveryScript(workspaceDir string, repos []types.GitHubRepoAccess) string {
	discoveryScript := buildRepoInstructionDiscoveryScript(workspaceDir, repos)
	if discoveryScript == "" {
		return ""
	}
	return fmt.Sprintf(`(
%s
) || echo "Warning: repo instruction discovery failed; continuing"
`, discoveryScript)
}

// buildGitHubCredentialHelper returns shell script lines that install a git
// credential helper on the VM if GitHub App is configured on the hub.
func buildGitHubCredentialHelper(cfg *types.HubConfig, hubURL, clawID string, repos []types.GitHubRepoAccess) string {
	if len(cfg.GitHubApps) == 0 {
		return "# GitHub App not configured — skipping credential helper"
	}
	clawToken := cfg.ClawToken
	tokenURL := fmt.Sprintf("%s/api/github/token/%s?claw_token=%s", hubURL, clawID, clawToken)
	return fmt.Sprintf(`# Install GitHub credential helper
set -euo pipefail
if [ -z "${HOME:-}" ]; then
  HOME="$(getent passwd "$(id -u)" | cut -d: -f6)"
  export HOME
fi
if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ]; then
  echo "ERROR: HOME is not set to a valid directory; cannot configure git credential helper" >&2
  exit 1
fi
echo "Configuring GitHub credential helper for user=$(whoami) home=$HOME"

sudo tee /usr/local/bin/elasticclaw-git-credentials > /dev/null << 'CREDEOF'
#!/bin/bash
# Git credential helper — fetches a fresh GitHub App installation token from the hub.
response=$(curl -sf %q)
if [ $? -ne 0 ] || [ -z "$response" ]; then
  exit 1
fi
token=$(echo "$response" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "protocol=https"
echo "host=github.com"
echo "username=x-access-token"
echo "password=$token"
CREDEOF
sudo chmod +x /usr/local/bin/elasticclaw-git-credentials

# Install git + gh CLI
if ! command -v git &>/dev/null; then
  echo "Installing git..."
  sudo apt-get update -qq
  sudo apt-get install -y git
fi

# Configure git to use the credential helper
git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials
git config --global --get-all credential.helper | grep -Fx /usr/local/bin/elasticclaw-git-credentials >/dev/null
git config --show-origin --global --get-all credential.helper

# Install gh CLI if possible. gh is useful, but git credential registration above is mandatory.
if ! command -v gh &>/dev/null; then
  (
    set +e
    echo "Installing gh CLI..."
    if curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null; then
      echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
      sudo apt-get update -qq && sudo apt-get install -y gh 2>/dev/null || echo "gh install failed, continuing"
    else
      echo "gh keyring install failed, continuing"
    fi
  ) || true
fi

# Configure gh to use the credential helper via GH_TOKEN env and wrapper.
if command -v gh &>/dev/null; then
  (
    set +e
%s
%s
  ) || echo "GitHub gh token refresh setup failed, continuing"
fi
echo "GitHub credential helper installed"

# Clone repos — non-fatal: token may not be available until bridge connects
# The agent can clone manually if this fails
cd "$HOME/.openclaw/workspace" || true
(
set +e
FAILED=0
%s
exit $FAILED
) || echo "Warning: repo clone failed — agent can retry after bridge connects"
%s`, tokenURL, buildGitHubTokenProfileInstallScript(), buildGitHubCLIWrapperInstallScript(), buildGitHubCloneScript(repos), buildBestEffortRepoInstructionDiscoveryScript("$HOME/.openclaw/workspace", repos))
}
