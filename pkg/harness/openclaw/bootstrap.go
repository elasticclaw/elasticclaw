package openclaw

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/harness"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type bootstrap struct{}

func init() {
	harness.RegisterBootstrap(bootstrap{})
}

func (bootstrap) Name() string { return harness.OpenClaw }

func (bootstrap) Config(in harness.BootstrapInput) harness.BootstrapConfig {
	return harness.BootstrapConfig{
		ProviderConfig: buildOpenClawProviderConfig(in.LLMKeys, in.SelectedKey),
		APIKeyAuthSync: buildOpenClawAPIKeyAuthSyncShell(in.LLMKeys, in.SelectedKey),
		OAuthAuthSync:  buildOpenClawOAuthAuthSyncShell(in.LLMKeys, in.SelectedKey),
		OnboardFlags:   buildOnboardFlags(in.LLMKeys, in.SelectedKey, in.DefaultModel),
	}
}

// buildOpenClawProviderConfig returns a python snippet that patches
// ~/.openclaw/openclaw.json with the agent default and gateway settings.
// Credentials are synced separately; see buildOpenClawAPIKeyAuthSyncShell.
// selectedKeyName is used to pick the active key (falls back to default, then first).
func buildOpenClawProviderConfig(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	// Determine active key
	activeKey := resolveActiveKey(keys, selectedKeyName)
	grokOAuth := activeKey != nil && activeKey.Provider == "grok" && activeKey.AuthProfile != "" && activeKey.APIKey == ""
	codexSelected := activeKey != nil && activeKey.Provider == "codex"
	openAISelected := activeKey != nil && activeKey.Provider == "openai"
	grokOAuthLiteral := "False"
	if grokOAuth {
		grokOAuthLiteral = "True"
	}
	codexSelectedLiteral := "False"
	if codexSelected {
		codexSelectedLiteral = "True"
	}
	openAISelectedLiteral := "False"
	if openAISelected {
		openAISelectedLiteral = "True"
	}

	return fmt.Sprintf(`python3 << 'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
os.makedirs(os.path.dirname(path), exist_ok=True)
try:
    with open(path) as f:
        config = json.load(f)
except FileNotFoundError:
    config = {}
except Exception:
    config = {}
model = os.environ.get('OPENCLAW_DEFAULT_MODEL', 'anthropic/claude-sonnet-4-6')
if %s and model.startswith('grok/'):
    model = 'xai/' + model.split('/', 1)[1]
if %s and model.startswith('codex/'):
    model = 'openai/' + model.split('/', 1)[1]
agent_defaults = config.setdefault('agents', {}).setdefault('defaults', {})
agent_defaults['model'] = model
agent_models = agent_defaults.get('models')
if isinstance(agent_models, dict):
    for key in list(agent_models.keys()):
        if 'kimi-k2p5' in key:
            agent_models.pop(key, None)
if %s:
    agent_defaults['thinkingDefault'] = 'medium'
    if not isinstance(agent_models, dict):
        agent_models = {}
        agent_defaults['models'] = agent_models
    model_config = agent_models.setdefault(model, {})
    if not isinstance(model_config, dict):
        model_config = {}
        agent_models[model] = model_config
    model_config['agentRuntime'] = {'id': 'codex'}
    config.setdefault('plugins', {}).setdefault('entries', {}).setdefault('codex', {})['enabled'] = True
if %s:
    if not isinstance(agent_models, dict):
        agent_models = {}
        agent_defaults['models'] = agent_models
    model_config = agent_models.setdefault(model, {})
    if not isinstance(model_config, dict):
        model_config = {}
        agent_models[model] = model_config
    model_config['agentRuntime'] = {'id': 'openclaw'}
# OpenClaw rejects the legacy top-level models provider catalog shape we used
# to write. Onboard handles provider auth; keep only agent default.
models = config.get('models')
if isinstance(models, dict) and any(k in models for k in ('providers', 'routers', 'mode')):
    config.pop('models', None)
if model.startswith('ollama/'):
    model_id = model.split('/', 1)[1]
    agent_defaults.setdefault('experimental', {})['localModelLean'] = True
    config.setdefault('models', {})['mode'] = 'merge'
    providers = config['models'].setdefault('providers', {})
    providers['ollama'] = {
        'baseUrl': 'http://ollama:11434',
        'api': 'ollama',
        'apiKey': 'OLLAMA_API_KEY',
        'models': [{
            'id': model_id,
            'name': model_id,
            'reasoning': False,
            'input': ['text'],
            'cost': {'input': 0, 'output': 0, 'cacheRead': 0, 'cacheWrite': 0},
            'contextWindow': 32768,
            'maxTokens': 1024,
            'params': {'num_ctx': 32768, 'thinking': False, 'keep_alive': '15m'},
            'compat': {'supportsTools': True, 'supportsUsageInStreaming': True},
        }],
    }
if model.startswith('grok/'):
    model_id = model.split('/', 1)[1]
    config.setdefault('models', {})['mode'] = 'merge'
    providers = config['models'].setdefault('providers', {})
    providers['grok'] = {
        'baseUrl': 'https://api.x.ai/v1',
        'api': 'openai',
        'apiKey': 'XAI_API_KEY',
        'models': [{
            'id': model_id,
            'name': model_id,
            'reasoning': True,
            'input': ['text', 'image'],
            'cost': {'input': 0, 'output': 0, 'cacheRead': 0, 'cacheWrite': 0},
            'contextWindow': 256000,
            'maxTokens': 32768,
            'compat': {'supportsTools': True, 'supportsUsageInStreaming': True},
        }],
    }
if model.startswith('fireworks/'):
    # OpenClaw 2026.9.x dropped the built-in Fireworks catalog (full-path
    # accounts/fireworks/models/* ids); register it as a custom provider so
    # the pinned model still resolves. The suffix is the native Fireworks
    # API model name. maxTokens covers reasoning plus the answer: at 8192
    # glm-5p3 spent the whole budget reasoning and never emitted a tool call.
    model_id = model.split('/', 1)[1]
    config.setdefault('models', {})['mode'] = 'merge'
    providers = config['models'].setdefault('providers', {})
    providers['fireworks'] = {
        'baseUrl': 'https://api.fireworks.ai/inference/v1',
        'api': 'openai-completions',
        'apiKey': '${FIREWORKS_API_KEY}',
        'models': [{
            'id': model_id,
            'name': model_id,
            'reasoning': True,
            'input': ['text'],
            'cost': {'input': 0, 'output': 0, 'cacheRead': 0, 'cacheWrite': 0},
            'contextWindow': 256000,
            'maxTokens': 32768,
            'compat': {'supportsTools': True, 'supportsUsageInStreaming': True},
        }],
    }
config.setdefault('gateway', {})['bind'] = 'loopback'
config['gateway']['port'] = 18789
gw_password = os.environ.get('ELASTICCLAW_GATEWAY_PASSWORD', '')
if gw_password:
    config['gateway']['auth'] = {'mode': 'password', 'password': gw_password}
    config['gateway']['remote'] = {'password': gw_password}
with open(path, 'w') as f:
    json.dump(config, f, indent=2)
print('OpenClaw config patched')
PYEOF`, grokOAuthLiteral, codexSelectedLiteral, codexSelectedLiteral, openAISelectedLiteral)
}

// buildOpenClawAPIKeyAuthSyncShell returns a shell snippet that persists
// direct API-key auth into OpenClaw's current auth store. OpenClaw 2026.9.4
// refuses every auth read ("requires legacy credential migration") while an
// Anthropic profile remains in the legacy auth-profiles.json, so the snippet
// first strips those profiles, e.g. from sandboxes restored from a checkpoint.
func buildOpenClawAPIKeyAuthSyncShell(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	activeKey := resolveActiveKey(keys, selectedKeyName)
	if activeKey == nil || activeKey.Provider != "anthropic" || !llmKeyHasRequiredAPIKey(activeKey) {
		return ""
	}
	envVar := activeKey.EnvVarName()
	return fmt.Sprintf(`if [ -n "${%s:-}" ]; then
  python3 - <<'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/agents/main/agent/auth-profiles.json')
try:
    with open(path) as f:
        auth = json.load(f)
except (FileNotFoundError, json.JSONDecodeError):
    auth = None
if isinstance(auth, dict):
    profiles = auth.get('profiles') or {}
    for name in [n for n, p in profiles.items() if isinstance(p, dict) and p.get('provider') == 'anthropic']:
        profiles.pop(name)
    (auth.get('order') or {}).pop('anthropic', None)
    if profiles:
        with open(path, 'w') as f:
            json.dump(auth, f, indent=2)
    else:
        os.remove(path)
PYEOF
  printf '%%s\n' "${%s}" | openclaw models auth paste-api-key --provider anthropic --profile-id anthropic:default
fi`, envVar, envVar)
}

// buildOpenClawOAuthAuthSyncShell returns a post-onboarding shell snippet that
// imports restored Grok CLI OAuth into OpenClaw's per-agent SQLite auth store.
// OpenClaw 2026.7.1-2 no longer reads credentials from auth-profiles.json at
// runtime, and its broad `doctor --fix` migration also changes unrelated config.
func buildOpenClawOAuthAuthSyncShell(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	activeKey := resolveActiveKey(keys, selectedKeyName)
	if activeKey == nil || activeKey.AuthProfile == "" || activeKey.APIKey != "" {
		return ""
	}
	if activeKey.Provider == "codex" {
		return buildOpenClawCodexOAuthAuthSyncShell()
	}
	if activeKey.Provider != "grok" {
		return ""
	}
	return `set -euo pipefail
# Let OpenClaw create and migrate its own SQLite schema. The short-lived
# placeholder is replaced below before any gateway or model process starts.
printf '%s\n' 'elasticclaw-auth-store-initializer' | openclaw models auth paste-token --provider xai --profile-id xai:default --expires-in 1m >/dev/null
node <<'NODE'
const fs = require('fs');
const path = require('path');
const { DatabaseSync } = require('node:sqlite');

const home = process.env.HOME;
const grokAuthPath = path.join(home, '.grok', 'auth.json');
const grokAuth = JSON.parse(fs.readFileSync(grokAuthPath, 'utf8'));
const source = Object.values(grokAuth).find((entry) =>
  entry && typeof entry === 'object' && entry.key && entry.refresh_token
);
if (!source) {
  throw new Error('restored Grok OAuth credential is missing access or refresh token');
}

const dbPath = path.join(home, '.openclaw', 'agents', 'main', 'agent', 'openclaw-agent.sqlite');
if (!fs.existsSync(dbPath)) {
  throw new Error('OpenClaw agent auth database does not exist after auth-store initialization');
}
const db = new DatabaseSync(dbPath);
const table = db.prepare(
  "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'auth_profile_store'"
).get();
if (!table) {
  throw new Error('OpenClaw agent auth database has no auth_profile_store table');
}

const existing = db.prepare(
  "SELECT store_json FROM auth_profile_store WHERE store_key = 'primary'"
).get();
const store = existing ? JSON.parse(existing.store_json) : { version: 1, profiles: {} };
store.version = 1;
if (!store.profiles || typeof store.profiles !== 'object') store.profiles = {};
const parsedExpires = Date.parse(source.expires_at || '');
if (!Number.isFinite(parsedExpires)) {
  throw new Error('restored Grok OAuth credential is missing a valid expires_at timestamp');
}
store.profiles['xai:default'] = {
  type: 'oauth',
  provider: 'xai',
  access: source.key,
  // The hub owns xAI's rotating refresh token. Giving the same token to
  // multiple claws lets the first local refresh revoke every other copy.
  refresh: 'elasticclaw-managed',
  expires: parsedExpires,
};

const now = Date.now();
db.exec('BEGIN IMMEDIATE');
try {
  db.prepare(
    'INSERT INTO auth_profile_store (store_key, store_json, updated_at) ' +
    'VALUES (?, ?, ?) ' +
    'ON CONFLICT(store_key) DO UPDATE SET ' +
    'store_json = excluded.store_json, updated_at = excluded.updated_at'
  ).run('primary', JSON.stringify(store), now);
  db.exec('COMMIT');
} catch (error) {
  db.exec('ROLLBACK');
  throw error;
} finally {
  db.close();
}
NODE
openclaw config set 'auth.profiles["xai:default"]' '{"provider":"xai","mode":"oauth"}' --strict-json >/dev/null
openclaw models auth list --provider xai --json | node -e '
let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { input += chunk; });
process.stdin.on("end", () => {
  const result = JSON.parse(input);
  if (!Array.isArray(result.profiles) || !result.profiles.some((profile) => profile.id === "xai:default" && profile.type === "oauth")) {
    throw new Error("OpenClaw did not load the migrated xai:default OAuth profile");
  }
});
'`
}

// buildOpenClawCodexOAuthAuthSyncShell verifies that OpenClaw discovers the
// restored Codex CLI credential as its canonical OpenAI auth profile. The
// current OpenClaw runtime reads openai:default from ~/.codex/auth.json, so this
// deliberately uses the supported CLI path instead of writing the SQLite store
// directly.
func buildOpenClawCodexOAuthAuthSyncShell() string {
	return `set -euo pipefail
node <<'NODE'
const fs = require('fs');
const path = require('path');

const authPath = path.join(process.env.HOME, '.codex', 'auth.json');
const auth = JSON.parse(fs.readFileSync(authPath, 'utf8'));
const tokens = auth && typeof auth === 'object' ? auth.tokens : null;
if (!tokens || typeof tokens.access_token !== 'string' || !tokens.access_token ||
    typeof tokens.refresh_token !== 'string' || !tokens.refresh_token) {
  throw new Error('restored Codex OAuth credential is missing access or refresh token');
}
NODE
openclaw models auth list --provider openai --json | node -e '
let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { input += chunk; });
process.stdin.on("end", () => {
  const result = JSON.parse(input);
  if (!Array.isArray(result.profiles) || !result.profiles.some((profile) =>
    profile.id === "openai:default" && profile.type === "oauth"
  )) {
    throw new Error("OpenClaw did not discover the restored Codex OAuth profile");
  }
});
'
openclaw config set 'auth.profiles["openai:default"]' '{"provider":"openai","mode":"oauth"}' --strict-json >/dev/null`
}

// buildOnboardFlags returns the --auth-choice flags for openclaw onboard based
// on the active LLM key (selected > default > first). OpenClaw 2026.9.4
// dropped the "anthropic-api-key" choice; Anthropic keys now use "apiKey".
func buildOnboardFlags(keys []*types.LLMKeyConfig, selectedKeyName, defaultModel string) string {
	active := resolveActiveKey(keys, selectedKeyName)
	if active == nil {
		return `--auth-choice apiKey --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"`
	}
	envVar := active.EnvVarName()
	switch active.Provider {
	case "anthropic":
		return fmt.Sprintf(`--auth-choice apiKey --anthropic-api-key "${%s:-placeholder}"`, envVar)
	case "fireworks":
		return fmt.Sprintf(`--auth-choice fireworks-api-key --fireworks-api-key "${%s:-}"`, envVar)
	case "openai":
		return fmt.Sprintf(`--auth-choice openai-api-key --openai-api-key "${%s:-}"`, envVar)
	case "groq":
		return fmt.Sprintf(`--auth-choice groq-api-key --groq-api-key "${%s:-}"`, envVar)
	case "deepseek":
		return fmt.Sprintf(`--auth-choice deepseek-api-key --deepseek-api-key "${%s:-}"`, envVar)
	case "codex":
		if active.AuthProfile != "" && active.APIKey == "" {
			return `--auth-choice skip`
		}
		return fmt.Sprintf(`--auth-choice openai-api-key --openai-api-key "${%s:-}"`, envVar)
	case "grok":
		if active.AuthProfile != "" && active.APIKey == "" {
			return `--auth-choice skip`
		}
		return fmt.Sprintf(`--auth-choice openai-api-key --openai-api-key "${%s:-}"`, envVar)
	case "ollama":
		model := active.DefaultModel
		if model == "" || !strings.HasPrefix(model, active.Provider+"/") {
			model = defaultModel
		}
		if model == "" || !strings.HasPrefix(model, active.Provider+"/") {
			model = "ollama/qwen2.5-coder:1.5b"
		}
		return fmt.Sprintf(`--auth-choice ollama --custom-base-url "http://ollama:11434" --custom-model-id %s`, shellQuote(stripProviderPrefix(model)))
	default:
		return `--auth-choice apiKey --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"`
	}
}

// resolveActiveKey selects the active key by selected name, then default, then first.
func resolveActiveKey(keys []*types.LLMKeyConfig, selectedKeyName string) *types.LLMKeyConfig {
	for _, k := range keys {
		if k.Name == selectedKeyName && llmKeyHasRequiredAPIKey(k) {
			return k
		}
	}
	for _, k := range keys {
		if k.Default && llmKeyHasRequiredAPIKey(k) {
			return k
		}
	}
	if len(keys) > 0 {
		for _, k := range keys {
			if llmKeyHasRequiredAPIKey(k) {
				return k
			}
		}
	}
	return nil
}

func llmKeyHasRequiredAPIKey(key *types.LLMKeyConfig) bool {
	if key == nil {
		return false
	}
	return key.APIKey != "" || key.Provider == "ollama" || key.AuthProfile != ""
}

// shellQuote returns a single-quoted shell string safe for embedding in scripts.
// Single quotes cannot appear inside single-quoted strings in bash, so we
// replace them with '"'"'.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func stripProviderPrefix(model string) string {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return model
}
