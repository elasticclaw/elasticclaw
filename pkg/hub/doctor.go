package hub

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// DoctorCheck is a single diagnostic check result.
type DoctorCheck struct {
	Category    string     `json:"category"` // "auth", "models", "sandboxes", "factories", "integrations", "mcp", "templates"
	Severity    string     `json:"severity"` // "critical", "warning", "info"
	Title       string     `json:"title"`
	Description string     `json:"description"`
	OK          bool       `json:"ok"`
	Error       string     `json:"error,omitempty"`
	FixAction   *FixAction `json:"fixAction,omitempty"` // nil if no auto-fix available
}

// FixAction describes an actionable fix the user can take.
type FixAction struct {
	Type   string            `json:"type"`   // "navigate", "set_field", "toggle"
	Target string            `json:"target"` // settings section path or object identifier
	Params map[string]string `json:"params,omitempty"`
	Label  string            `json:"label"` // button text
}

// DoctorResponse is the full diagnostic report.
type DoctorResponse struct {
	Checks  []DoctorCheck `json:"checks"`
	Summary struct {
		Total    int `json:"total"`
		Critical int `json:"critical"`
		Warning  int `json:"warning"`
		Info     int `json:"info"`
		Passed   int `json:"passed"`
	} `json:"summary"`
	CachedAt *time.Time `json:"cachedAt,omitempty"`
}

// doctorCache holds the last computed report and its timestamp.
type doctorCache struct {
	report DoctorResponse
	at     time.Time
}

var (
	lastDoctorReport *doctorCache
	doctorMu         sync.RWMutex
)

// handleDoctor handles GET /api/doctor.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check cache unless ?refresh=true
	refresh := r.URL.Query().Get("refresh") == "true"
	doctorMu.RLock()
	cached := lastDoctorReport
	doctorMu.RUnlock()
	if !refresh && cached != nil && time.Since(cached.at) < 5*time.Minute {
		resp := cached.report
		resp.CachedAt = &cached.at
		jsonOK(w, resp)
		return
	}

	report := s.runDoctorChecks(r.Context())
	doctorMu.Lock()
	lastDoctorReport = &doctorCache{report: report, at: time.Now()}
	doctorMu.Unlock()
	jsonOK(w, report)
}

// runDoctorChecks runs all diagnostic checks and returns the report.
func (s *Server) runDoctorChecks(ctx context.Context) DoctorResponse {
	var checks []DoctorCheck

	s.mu.RLock()
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	// Load disk config for secrets, integrations, etc.
	diskCfg, _ := config.LoadHubConfig()
	if diskCfg == nil {
		diskCfg = hubCfg
	}

	// --- Models / LLM Keys ---
	checks = append(checks, s.checkLLMKeys(hubCfg)...)
	checks = append(checks, s.checkDefaultModel(hubCfg)...)

	// --- Sandboxes / Providers ---
	checks = append(checks, s.checkProviders(hubCfg)...)

	// --- Factories ---
	checks = append(checks, s.checkFactories(hubCfg, diskCfg)...)

	// --- Templates ---
	checks = append(checks, s.checkTemplates(hubCfg, diskCfg)...)

	// --- Integrations ---
	checks = append(checks, s.checkIntegrations(hubCfg, diskCfg)...)

	// --- GitHub Apps ---
	checks = append(checks, s.checkGitHubApps(hubCfg)...)

	// --- MCP Servers ---
	checks = append(checks, s.checkMCPServers(hubCfg, diskCfg)...)

	// --- Auth ---
	checks = append(checks, s.checkAuth(hubCfg)...)

	// --- Notifications ---
	checks = append(checks, s.checkNotifications(hubCfg)...)
	checks = append(checks, s.checkNotifyActions(hubCfg)...)

	// --- Hub Settings ---
	checks = append(checks, s.checkHubSettings(hubCfg)...)

	// --- Runtime state ---
	checks = append(checks, s.checkRuntimeState(ctx)...)

	// Compute summary: total = all checks, passed = OK checks
	var report DoctorResponse
	report.Checks = checks
	for _, c := range checks {
		report.Summary.Total++
		if c.OK {
			report.Summary.Passed++
		} else {
			switch c.Severity {
			case "critical":
				report.Summary.Critical++
			case "warning":
				report.Summary.Warning++
			case "info":
				report.Summary.Info++
			}
		}
	}
	return report
}

// ==================== LLM KEY CHECKS ====================

func (s *Server) checkLLMKeys(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if len(cfg.LLMKeys) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "models",
			Severity:    "critical",
			Title:       "No LLM keys configured",
			Description: "At least one LLM key is required for agents to function.",
			OK:          false,
			FixAction: &FixAction{
				Type:   "navigate",
				Target: "/settings/models",
				Label:  "Add LLM Key",
			},
		})
		return checks
	}

	// Check each key for valid provider
	validProviders := map[string]bool{
		"anthropic": true, "openai": true, "fireworks": true,
		"moonshot": true, "google": true, "mistral": true,
		"groq": true, "grok": true, "deepseek": true, "codex": true, "ollama": true,
	}

	allKeysValid := true
	for _, key := range cfg.LLMKeys {
		if !validProviders[key.Provider] {
			allKeysValid = false
			checks = append(checks, DoctorCheck{
				Category:    "models",
				Severity:    "warning",
				Title:       fmt.Sprintf("Unknown LLM provider: %q", key.Provider),
				Description: fmt.Sprintf("LLM key %q uses provider %q which is not recognized.", key.Name, key.Provider),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/models",
					Label:  "Edit LLM Key",
				},
			})
		}
		if !llmKeyHasRequiredAPIKey(key) {
			allKeysValid = false
			checks = append(checks, DoctorCheck{
				Category:    "models",
				Severity:    "critical",
				Title:       fmt.Sprintf("LLM key %q has no API key", key.Name),
				Description: fmt.Sprintf("LLM key %q is configured but the API key is empty.", key.Name),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/models",
					Label:  "Set API Key",
				},
			})
		}
	}

	// Check for a default key
	hasDefault := false
	for _, key := range cfg.LLMKeys {
		if key.Default {
			hasDefault = true
			break
		}
	}
	if !hasDefault && len(cfg.LLMKeys) > 0 {
		checks = append(checks, DoctorCheck{
			Category:    "models",
			Severity:    "info",
			Title:       "No default LLM key set",
			Description: "No LLM key is marked as default. The first key will be used, but explicit default is recommended.",
			OK:          false,
			FixAction: &FixAction{
				Type:   "navigate",
				Target: "/settings/models",
				Label:  "Set Default",
			},
		})
	} else if allKeysValid {
		checks = append(checks, DoctorCheck{
			Category:    "models",
			Severity:    "info",
			Title:       "LLM keys configured",
			Description: fmt.Sprintf("%d LLM key(s) configured and valid.", len(cfg.LLMKeys)),
			OK:          true,
		})
	}

	return checks
}

func (s *Server) checkDefaultModel(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if cfg.DefaultModel == "" {
		// Find the default LLM key (explicitly marked default, or first key as fallback)
		var defaultKey *types.LLMKeyConfig
		for _, k := range cfg.LLMKeys {
			if k.Default {
				defaultKey = k
				break
			}
		}
		if defaultKey == nil && len(cfg.LLMKeys) > 0 {
			defaultKey = cfg.LLMKeys[0]
		}

		if defaultKey != nil {
			// Delegate to the same resolution logic used at runtime to avoid false positives
			resolved := resolveDefaultModelForKey(cfg, defaultKey)
			if resolved != "" {
				checks = append(checks, DoctorCheck{
					Category:    "models",
					Severity:    "info",
					Title:       "Default model configured",
					Description: fmt.Sprintf("Default model resolves to %q (from key %q). No hub-level default_model is set, which is fine.", resolved, defaultKey.Name),
					OK:          true,
				})
			} else {
				checks = append(checks, DoctorCheck{
					Category:    "models",
					Severity:    "warning",
					Title:       "No default model configured",
					Description: fmt.Sprintf("LLM key %q (provider %q) has no default model and no hub-level default_model is set. Set a default model on the key or configure hub-level default_model.", defaultKey.Name, defaultKey.Provider),
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/models",
						Label:  "Set Default Model",
					},
				})
			}
		} else {
			// No LLM keys at all — this is already flagged by checkLLMKeys as critical
			checks = append(checks, DoctorCheck{
				Category:    "models",
				Severity:    "info",
				Title:       "No default model configured",
				Description: "No default model is set and no LLM keys are configured.",
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/models",
					Label:  "Set Default Model",
				},
			})
		}
	} else if !strings.Contains(cfg.DefaultModel, "/") {
		checks = append(checks, DoctorCheck{
			Category:    "models",
			Severity:    "warning",
			Title:       "Default model format is invalid",
			Description: fmt.Sprintf("Default model %q should be in format provider/model (e.g., anthropic/claude-sonnet-4-6).", cfg.DefaultModel),
			OK:          false,
			FixAction: &FixAction{
				Type:   "set_field",
				Target: "default_model",
				Params: map[string]string{"hint": "provider/model"},
				Label:  "Fix Format",
			},
		})
	} else {
		checks = append(checks, DoctorCheck{
			Category:    "models",
			Severity:    "info",
			Title:       "Default model configured",
			Description: fmt.Sprintf("Default model is set to %q.", cfg.DefaultModel),
			OK:          true,
		})
	}

	return checks
}

// ==================== PROVIDER CHECKS ====================

func (s *Server) checkProviders(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if len(cfg.Providers) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "sandboxes",
			Severity:    "critical",
			Title:       "No sandbox providers configured",
			Description: "At least one sandbox provider must be configured.",
			OK:          false,
			FixAction: &FixAction{
				Type:   "navigate",
				Target: "/settings/runtimes",
				Label:  "Add Provider",
			},
		})
		return checks
	}

	validProviders := map[string]bool{
		"daytona": true, "replicated": true, "exedev": true, "docker": true, "lambda-microvms": true,
	}

	allProvidersValid := true
	for name, p := range cfg.Providers {
		if !validProviders[name] {
			allProvidersValid = false
			checks = append(checks, DoctorCheck{
				Category:    "sandboxes",
				Severity:    "warning",
				Title:       fmt.Sprintf("Unknown sandbox provider: %q", name),
				Description: fmt.Sprintf("Provider %q is not a recognised sandbox provider.", name),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/runtimes",
					Label:  "Fix Provider",
				},
			})
			continue
		}
		switch name {
		case "daytona":
			if p.APIKey == "" {
				allProvidersValid = false
				checks = append(checks, DoctorCheck{
					Category:    "sandboxes",
					Severity:    "critical",
					Title:       "Daytona provider missing API key",
					Description: "The Daytona sandbox provider is configured but the API key is empty.",
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/runtimes",
						Label:  "Configure Daytona",
					},
				})
			}
		case "replicated":
			if p.Token == "" {
				allProvidersValid = false
				checks = append(checks, DoctorCheck{
					Category:    "sandboxes",
					Severity:    "critical",
					Title:       "Replicated provider missing token",
					Description: "The Replicated sandbox provider is configured but the token is empty.",
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/runtimes",
						Label:  "Configure Replicated",
					},
				})
			}
		case "exedev":
			// exe.dev uses SSH key authentication; no explicit API key needed in config
		case "lambda-microvms":
			if p.ImageIdentifier == "" {
				allProvidersValid = false
				checks = append(checks, DoctorCheck{
					Category:    "sandboxes",
					Severity:    "critical",
					Title:       "AWS Lambda MicroVMs provider missing image identifier",
					Description: "The AWS Lambda MicroVMs sandbox provider is configured but the image identifier is empty.",
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/runtimes",
						Label:  "Configure AWS Lambda MicroVMs",
					},
				})
			}
		}
	}
	if allProvidersValid {
		checks = append(checks, DoctorCheck{
			Category:    "sandboxes",
			Severity:    "info",
			Title:       "Sandbox providers configured",
			Description: fmt.Sprintf("%d sandbox provider(s) configured with credentials.", len(cfg.Providers)),
			OK:          true,
		})
	}

	return checks
}

// ==================== FACTORY CHECKS ====================

func (s *Server) checkFactories(cfg *types.HubConfig, diskCfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	factories, _ := loadExternalFactories()
	if len(factories) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "factories",
			Severity:    "info",
			Title:       "No factories configured",
			Description: "No factories are configured. Factory-based agent creation is disabled.",
			OK:          true,
		})
		return checks
	}

	// Build set of template names from external storage + legacy DB
	templateNames := make(map[string]bool)
	templateNamesValid := true

	// External templates first
	externalNames, err := listExternalTemplates()
	if err == nil {
		for _, name := range externalNames {
			templateNames[name] = true
		}
	}

	// Legacy DB templates (for migration period)
	rows, err := s.db.Query(`SELECT name FROM hub_templates`)
	if err != nil {
		// Query itself failed — templateNames may be empty from external too,
		// so we must skip template-existence checks to avoid false critical alerts.
		if len(templateNames) == 0 {
			templateNamesValid = false
			checks = append(checks, DoctorCheck{
				Category:    "factories",
				Severity:    "warning",
				Title:       "Could not verify templates",
				Description: fmt.Sprintf("Template list query failed: %v. Factory template references cannot be validated.", err),
				OK:          false,
			})
		}
	} else if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				templateNames[name] = true
			}
		}
		if err := rows.Err(); err != nil {
			// DB iteration failed — templateNames may be incomplete.
			// Emit a warning and skip the template-existence factory check
			// to avoid false "missing template" critical alerts.
			if len(templateNames) == 0 {
				templateNamesValid = false
				checks = append(checks, DoctorCheck{
					Category:    "factories",
					Severity:    "warning",
					Title:       "Could not verify templates",
					Description: fmt.Sprintf("Template list query failed during iteration: %v. Factory template references cannot be validated.", err),
					OK:          false,
				})
			}
		}
	}
	// Also check local templates via config resolution
	// (templates are directories, not a list in hub.yaml)

	// Build set of secret names
	secretNames := make(map[string]bool)
	for name := range diskCfg.Secrets {
		secretNames[name] = true
	}

	// Track factory names for duplicates
	factoryNames := make(map[string]int)

	// Track triggers for overlap detection
	type triggerKey struct {
		integration string
		workspace   string
		trigger     string
	}
	triggers := make(map[triggerKey][]string)

	for _, f := range factories {
		if f == nil {
			continue
		}

		// Skip disabled factories
		if !isFactoryEnabled(f) {
			continue
		}

		// Check 1: duplicate name
		factoryNames[f.Name]++

		// Check 2: template exists
		if templateNamesValid && f.Template != "" && !templateNames[f.Template] {
			checks = append(checks, DoctorCheck{
				Category:    "factories",
				Severity:    "critical",
				Title:       fmt.Sprintf("Factory %q references missing template", f.Name),
				Description: fmt.Sprintf("Factory %q references template %q which is not found in pushed templates or local templates.", f.Name, f.Template),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/templates",
					Label:  "Push Template",
				},
			})
		}

		// Check 3: webhook secret
		if f.WebhookSecret == "" && f.WebhookSecretRef == "" {
			checks = append(checks, DoctorCheck{
				Category:    "factories",
				Severity:    "warning",
				Title:       fmt.Sprintf("Factory %q has no webhook secret", f.Name),
				Description: fmt.Sprintf("Factory %q has neither an inline webhook_secret nor a webhook_secret_ref. Webhooks from the integration will not be validated.", f.Name),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/secrets",
					Label:  "Add Secret",
				},
			})
		} else if f.WebhookSecretRef != "" && !secretNames[f.WebhookSecretRef] {
			checks = append(checks, DoctorCheck{
				Category:    "factories",
				Severity:    "warning",
				Title:       fmt.Sprintf("Factory %q references missing secret", f.Name),
				Description: fmt.Sprintf("Factory %q references webhook_secret_ref %q which is not in the secrets map.", f.Name, f.WebhookSecretRef),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/secrets",
					Label:  "Create Secret",
				},
			})
		}

		// Check 4: factory secret_refs point to existing secrets
		for envName, secretRef := range f.SecretRefs {
			if !secretNames[secretRef] {
				checks = append(checks, DoctorCheck{
					Category:    "factories",
					Severity:    "warning",
					Title:       fmt.Sprintf("Factory %q secret_refs references missing secret", f.Name),
					Description: fmt.Sprintf("Factory %q secret_refs maps %q to secret %q which is not in the secrets map.", f.Name, envName, secretRef),
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/secrets",
						Label:  "Create Secret",
					},
				})
			}
		}

		// Check 5: overlapping triggers
		var key triggerKey
		switch f.Integration {
		case "linear", "shortcut", "github-issues", "jira":
			key = triggerKey{integration: f.Integration, workspace: f.Workspace, trigger: f.TriggerStatus}
		case "github":
			if f.Trigger != nil {
				key = triggerKey{integration: f.Integration, workspace: strings.Join(f.Repos, ","), trigger: f.Trigger.On + "/" + f.Trigger.Action}
			}
		}
		if key.integration != "" {
			triggers[key] = append(triggers[key], f.Name)
		}
	}

	// Report duplicate names
	for name, count := range factoryNames {
		if count > 1 {
			checks = append(checks, DoctorCheck{
				Category:    "factories",
				Severity:    "warning",
				Title:       fmt.Sprintf("Duplicate factory name: %q", name),
				Description: fmt.Sprintf("There are %d factories named %q. Only the last one will be used.", count, name),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/factories",
					Label:  "Rename Factory",
				},
			})
		}
	}

	// Report overlapping triggers
	for key, names := range triggers {
		if len(names) > 1 {
			checks = append(checks, DoctorCheck{
				Category:    "factories",
				Severity:    "warning",
				Title:       fmt.Sprintf("Overlapping factory triggers: %s", strings.Join(names, ", ")),
				Description: fmt.Sprintf("Factories %s all trigger on the same event (integration=%s, workspace=%s, trigger=%s). This will create duplicate agents.", strings.Join(names, ", "), key.integration, key.workspace, key.trigger),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/factories",
					Label:  "Fix Triggers",
				},
			})
		}
	}

	if len(checks) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "factories",
			Severity:    "info",
			Title:       "Factories configured",
			Description: fmt.Sprintf("%d factory(ies) configured with no issues detected.", len(factories)),
			OK:          true,
		})
	}

	return checks
}

// ==================== TEMPLATE CHECKS ====================

func (s *Server) checkTemplates(cfg *types.HubConfig, diskCfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	// Build set of secret names
	secretNames := make(map[string]bool)
	for name := range diskCfg.Secrets {
		secretNames[name] = true
	}

	// Check external templates for common issues
	externalNames, err := listExternalTemplates()
	if err != nil && !os.IsNotExist(err) {
		checks = append(checks, DoctorCheck{
			Category:    "templates",
			Severity:    "warning",
			Title:       "Could not list external templates",
			Description: fmt.Sprintf("Error reading templates directory: %v", err),
			OK:          false,
		})
	} else {
		for _, name := range externalNames {
			_, err := loadExternalTemplate(name)
			if err != nil {
				checks = append(checks, DoctorCheck{
					Category:    "templates",
					Severity:    "warning",
					Title:       fmt.Sprintf("Template %q unreadable", name),
					Description: fmt.Sprintf("Could not read template %q: %v", name, err),
					OK:          false,
				})
				continue
			}
			// Check for elasticclaw-config.yaml directly (not via ReadTemplateFiles allow-list)
			configPath := filepath.Join(templatesDir(), name, "elasticclaw-config.yaml")
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				checks = append(checks, DoctorCheck{
					Category:    "templates",
					Severity:    "warning",
					Title:       fmt.Sprintf("Template %q missing elasticclaw-config.yaml", name),
					Description: fmt.Sprintf("Template %q does not contain an elasticclaw-config.yaml file. This may cause issues when creating agents.", name),
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/templates",
						Label:  "Manage Templates",
					},
				})
			} else {
				// Check for deprecated secrets: list format
				if data, err := os.ReadFile(configPath); err == nil {
					tmplCfg, parseErr := config.ParseTemplateConfig(data)
					if parseErr == nil && tmplCfg != nil {
						if len(tmplCfg.Secrets) > 0 {
							checks = append(checks, DoctorCheck{
								Category:    "templates",
								Severity:    "warning",
								Title:       fmt.Sprintf("Template %q uses deprecated secrets: list", name),
								Description: fmt.Sprintf("Template %q uses the deprecated 'secrets:' list format. Migrate to 'secret_refs:' map.\n\nExample:\n  secret_refs:\n    LINEAR_API_KEY: linear_api_key\n\nDocs: https://www.elasticclaw.ai/docs/troubleshooting", name),
								OK:          false,
							})
						}
						// Check for missing secret_refs references
						for envName, secretRef := range tmplCfg.SecretRefs {
							if !secretNames[secretRef] {
								checks = append(checks, DoctorCheck{
									Category:    "templates",
									Severity:    "warning",
									Title:       fmt.Sprintf("Template %q secret_refs references missing secret", name),
									Description: fmt.Sprintf("Template %q secret_refs maps %q to secret %q which is not in the secrets map.", name, envName, secretRef),
									OK:          false,
									FixAction: &FixAction{
										Type:   "navigate",
										Target: "/settings/secrets",
										Label:  "Create Secret",
									},
								})
							}
						}
					}
				}
			}
		}
		if len(externalNames) == 0 {
			checks = append(checks, DoctorCheck{
				Category:    "templates",
				Severity:    "info",
				Title:       "No external templates",
				Description: "No templates found in external storage. Use 'elasticclaw template push' to add templates.",
				OK:          true,
			})
		} else {
			checks = append(checks, DoctorCheck{
				Category:    "templates",
				Severity:    "info",
				Title:       fmt.Sprintf("%d template(s) in external storage", len(externalNames)),
				Description: "Templates are stored as external files alongside hub.yaml.",
				OK:          true,
			})
		}
	}

	return checks
}

// ==================== INTEGRATION CHECKS ====================

func (s *Server) checkIntegrations(cfg *types.HubConfig, diskCfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if cfg.Integrations == nil {
		return checks
	}

	// Build set of factory workspaces for cross-reference
	factoryWorkspaces := make(map[string]map[string]bool) // integration -> workspace -> exists
	factories, _ := loadExternalFactories()
	for _, f := range factories {
		if f == nil || !isFactoryEnabled(f) {
			continue
		}
		if factoryWorkspaces[f.Integration] == nil {
			factoryWorkspaces[f.Integration] = make(map[string]bool)
		}
		factoryWorkspaces[f.Integration][f.Workspace] = true
	}

	// Check Linear
	for _, li := range cfg.Integrations.Linear {
		if li.Token == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "critical",
				Title:       fmt.Sprintf("Linear workspace %q missing token", li.Workspace),
				Description: fmt.Sprintf("Linear integration workspace %q has no API token configured.", li.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Token",
				},
			})
		} else {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("Linear workspace %q token configured", li.Workspace),
				Description: fmt.Sprintf("Linear workspace %q has an API token configured.", li.Workspace),
				OK:          true,
			})
		}
		if li.WebhookSecret == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "warning",
				Title:       fmt.Sprintf("Linear workspace %q missing webhook secret", li.Workspace),
				Description: fmt.Sprintf("Linear integration workspace %q has no webhook secret. Webhooks will not be validated.", li.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Secret",
				},
			})
		}
		// Check if any factory uses this workspace
		if !factoryWorkspaces["linear"][li.Workspace] {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("Linear workspace %q has no factory using it", li.Workspace),
				Description: fmt.Sprintf("Linear workspace %q is configured but no factory references it.", li.Workspace),
				OK:          false,
			})
		}
	}

	// Check Shortcut
	for _, si := range cfg.Integrations.Shortcut {
		if si.Token == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "critical",
				Title:       fmt.Sprintf("Shortcut workspace %q missing token", si.Workspace),
				Description: fmt.Sprintf("Shortcut integration workspace %q has no API token configured.", si.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Token",
				},
			})
		} else {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("Shortcut workspace %q token configured", si.Workspace),
				Description: fmt.Sprintf("Shortcut workspace %q has an API token configured.", si.Workspace),
				OK:          true,
			})
		}

		if !factoryWorkspaces["shortcut"][si.Workspace] {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("Shortcut workspace %q has no factory using it", si.Workspace),
				Description: fmt.Sprintf("Shortcut workspace %q is configured but no factory references it.", si.Workspace),
				OK:          false,
			})
		}
	}

	// Check GitHub Issues
	for _, gi := range cfg.Integrations.GitHubIssues {
		if gi.Token == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "critical",
				Title:       fmt.Sprintf("GitHub Issues workspace %q missing token", gi.Workspace),
				Description: fmt.Sprintf("GitHub Issues integration workspace %q has no personal access token configured.", gi.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Token",
				},
			})
		} else {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("GitHub Issues workspace %q token configured", gi.Workspace),
				Description: fmt.Sprintf("GitHub Issues workspace %q has a personal access token configured.", gi.Workspace),
				OK:          true,
			})
		}
		if gi.WebhookSecret == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "warning",
				Title:       fmt.Sprintf("GitHub Issues workspace %q missing webhook secret", gi.Workspace),
				Description: fmt.Sprintf("GitHub Issues workspace %q has no webhook secret. Webhooks will not be validated.", gi.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Secret",
				},
			})
		}
		if !factoryWorkspaces["github-issues"][gi.Workspace] {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("GitHub Issues workspace %q has no factory using it", gi.Workspace),
				Description: fmt.Sprintf("GitHub Issues workspace %q is configured but no factory references it.", gi.Workspace),
				OK:          false,
			})
		}
	}

	for _, ji := range cfg.Integrations.Jira {
		if ji.Token == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "critical",
				Title:       fmt.Sprintf("Jira workspace %q missing token", ji.Workspace),
				Description: fmt.Sprintf("Jira integration workspace %q has no API token configured.", ji.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Token",
				},
			})
		}
		if ji.BaseURL == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "critical",
				Title:       fmt.Sprintf("Jira workspace %q missing base URL", ji.Workspace),
				Description: fmt.Sprintf("Jira integration workspace %q has no base URL configured.", ji.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Base URL",
				},
			})
		}
		if ji.Token != "" && ji.BaseURL != "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("Jira workspace %q configured", ji.Workspace),
				Description: fmt.Sprintf("Jira workspace %q has a base URL and token configured.", ji.Workspace),
				OK:          true,
			})
		}
		if ji.WebhookSecret == "" {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "warning",
				Title:       fmt.Sprintf("Jira workspace %q missing webhook secret", ji.Workspace),
				Description: fmt.Sprintf("Jira workspace %q has no webhook secret. Webhooks will not be validated.", ji.Workspace),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/issue-trackers",
					Label:  "Add Secret",
				},
			})
		}
		if !factoryWorkspaces["jira"][ji.Workspace] {
			checks = append(checks, DoctorCheck{
				Category:    "integrations",
				Severity:    "info",
				Title:       fmt.Sprintf("Jira workspace %q has no factory using it", ji.Workspace),
				Description: fmt.Sprintf("Jira workspace %q is configured but no factory references it.", ji.Workspace),
				OK:          false,
			})
		}
	}

	if len(cfg.Integrations.Linear) == 0 && len(cfg.Integrations.Shortcut) == 0 && len(cfg.Integrations.GitHubIssues) == 0 && len(cfg.Integrations.Jira) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "integrations",
			Severity:    "info",
			Title:       "No integrations configured",
			Description: "No issue tracker integrations are configured.",
			OK:          true,
		})
	}

	return checks
}

// ==================== GITHUB APP CHECKS ====================

func (s *Server) checkGitHubApps(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if len(cfg.GitHubApps) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "github",
			Severity:    "info",
			Title:       "No GitHub Apps configured",
			Description: "No GitHub Apps are configured. GitHub App-based authentication is disabled.",
			OK:          true,
		})
		return checks
	}

	allAppsValid := true
	for _, app := range cfg.GitHubApps {
		if app.PrivateKeyPEM == "" {
			allAppsValid = false
			checks = append(checks, DoctorCheck{
				Category:    "github",
				Severity:    "critical",
				Title:       fmt.Sprintf("GitHub App %d missing private key", app.AppID),
				Description: fmt.Sprintf("GitHub App %d has no private key configured. Installation tokens cannot be minted.", app.AppID),
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/github",
					Label:  "Add Private Key",
				},
			})
		}
		if app.AppID == 0 {
			allAppsValid = false
			checks = append(checks, DoctorCheck{
				Category:    "github",
				Severity:    "critical",
				Title:       "GitHub App missing App ID",
				Description: "A GitHub App is configured but has no App ID.",
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/github",
					Label:  "Configure App",
				},
			})
		}
	}
	if allAppsValid {
		checks = append(checks, DoctorCheck{
			Category:    "github",
			Severity:    "info",
			Title:       "GitHub Apps configured",
			Description: fmt.Sprintf("%d GitHub App(s) configured with valid credentials.", len(cfg.GitHubApps)),
			OK:          true,
		})
	}

	return checks
}

// ==================== MCP SERVER CHECKS ====================

func (s *Server) checkMCPServers(cfg *types.HubConfig, diskCfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if len(cfg.MCPServers) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "mcp",
			Severity:    "info",
			Title:       "No MCP servers configured",
			Description: "No MCP servers are configured.",
			OK:          true,
		})
		return checks
	}

	// Build set of secret names
	secretNames := make(map[string]bool)
	for name := range diskCfg.Secrets {
		secretNames[name] = true
	}

	allMCPValid := true
	for _, mcp := range cfg.MCPServers {
		if mcp == nil {
			continue
		}

		// Check secrets referenced
		for envVar, secretRef := range mcp.Secrets {
			if !secretNames[secretRef] {
				allMCPValid = false
				checks = append(checks, DoctorCheck{
					Category:    "mcp",
					Severity:    "warning",
					Title:       fmt.Sprintf("MCP server %q references missing secret", mcp.Name),
					Description: fmt.Sprintf("MCP server %q references secret %q (env var %s) which is not in the secrets map.", mcp.Name, secretRef, envVar),
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/secrets",
						Label:  "Create Secret",
					},
				})
			}
		}
	}
	if allMCPValid {
		checks = append(checks, DoctorCheck{
			Category:    "mcp",
			Severity:    "info",
			Title:       "MCP servers configured",
			Description: fmt.Sprintf("%d MCP server(s) configured with valid secrets.", len(cfg.MCPServers)),
			OK:          true,
		})
	}

	return checks
}

// ==================== AUTH CHECKS ====================

func (s *Server) checkAuth(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	// Check for lockout risk
	hasPassword := cfg.UIPassword != "" || (cfg.Auth != nil && !cfg.Auth.DisablePasswordAuth)
	hasOAuth := cfg.Auth != nil && cfg.Auth.GitHubOAuth != nil && cfg.Auth.GitHubOAuth.ClientID != ""

	if !hasPassword && !hasOAuth {
		checks = append(checks, DoctorCheck{
			Category:    "auth",
			Severity:    "critical",
			Title:       "Authentication lockout risk",
			Description: "Both password auth and GitHub OAuth are disabled/unconfigured. You will be locked out of the web UI.",
			OK:          false,
			FixAction: &FixAction{
				Type:   "navigate",
				Target: "/settings/authentication",
				Label:  "Configure Auth",
			},
		})
		return checks
	}

	authIssues := 0
	if cfg.Auth != nil && cfg.Auth.GitHubOAuth != nil {
		if cfg.Auth.GitHubOAuth.ClientID != "" && cfg.Auth.GitHubOAuth.ClientSecret == "" {
			authIssues++
			checks = append(checks, DoctorCheck{
				Category:    "auth",
				Severity:    "warning",
				Title:       "GitHub OAuth missing client secret",
				Description: "GitHub OAuth Client ID is set but the client secret is empty.",
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/authentication",
					Label:  "Add Secret",
				},
			})
		}
		if cfg.Auth.GitHubOAuth.ClientID != "" &&
			len(cfg.Auth.GitHubOAuth.AllowedUsers) == 0 &&
			len(cfg.Auth.GitHubOAuth.AllowedOrgs) == 0 &&
			len(cfg.Auth.GitHubOAuth.AllowedTeams) == 0 {
			authIssues++
			checks = append(checks, DoctorCheck{
				Category:    "auth",
				Severity:    "warning",
				Title:       "GitHub OAuth has no access restrictions",
				Description: "GitHub OAuth is configured but no allowed users, orgs, or teams are set. Any GitHub user can sign in.",
				OK:          false,
				FixAction: &FixAction{
					Type:   "navigate",
					Target: "/settings/authentication",
					Label:  "Set Access Control",
				},
			})
		}
	}

	if authIssues == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "auth",
			Severity:    "info",
			Title:       "Authentication configured",
			Description: "At least one authentication method is configured.",
			OK:          true,
		})
	}

	return checks
}

// ==================== HUB SETTINGS CHECKS ====================

// ==================== NOTIFICATION CHECKS ====================

// checkNotifications validates the notifications config the way delivery
// would: structural invariants first, then each named notifier is actually
// constructed through the provider registry, which validates the type, the
// provider settings and resolves the token secret. Without this the only
// signal for a broken notifier is a single log line from the poller.
func (s *Server) checkNotifications(cfg *types.HubConfig) []DoctorCheck {
	nCfg := cfg.Notifications
	if nCfg == nil {
		// Feature absent — nothing to report.
		return nil
	}
	var checks []DoctorCheck
	// The two per-feature validators, not the composed one: each minute tick
	// gates on the validity of its own feature's config only, so a defect
	// confined to the lifecycle block pauses lifecycle alerts while scheduled
	// reports keep delivering, and vice versa. One composed "no notifications
	// will be delivered" row would be wrong in both directions; these two rows
	// each describe exactly what its tick's gate actually pauses, mirroring
	// the two tick log lines.
	if err := types.ValidateLifecycleNotificationsConfig(nCfg); err != nil {
		checks = append(checks, DoctorCheck{
			Category:    "notifications",
			Severity:    "critical",
			Title:       "Lifecycle notifications config invalid",
			Description: "No lifecycle notifications will be delivered until this is fixed in hub.yaml.",
			OK:          false,
			Error:       err.Error(),
		})
	}
	if err := types.ValidateScheduledNotificationsConfig(nCfg); err != nil {
		checks = append(checks, DoctorCheck{
			Category:    "notifications",
			Severity:    "critical",
			Title:       "Scheduled reports config invalid",
			Description: "No scheduled reports will be delivered until this is fixed in hub.yaml.",
			OK:          false,
			Error:       err.Error(),
		})
	}

	secrets := func(name string) (string, bool) {
		v, ok := cfg.Secrets[name]
		return v, ok
	}
	names := make([]string, 0, len(nCfg.Notifiers))
	for name := range nCfg.Notifiers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		nc := nCfg.Notifiers[name]
		if !notify.Supported(nc.Type) {
			checks = append(checks, DoctorCheck{
				Category:    "notifications",
				Severity:    "critical",
				Title:       fmt.Sprintf("Notifier %q has unsupported type %q", name, nc.Type),
				Description: "Messages routed through this notifier will never be delivered.",
				OK:          false,
			})
			continue
		}
		// Constructing the notifier runs the provider's own config checks and
		// resolves its secrets — exactly what a real send does first.
		if _, err := notify.New(nc.Type, s.notifierSettings(nc), secrets); err != nil {
			checks = append(checks, DoctorCheck{
				Category:    "notifications",
				Severity:    "critical",
				Title:       fmt.Sprintf("Notifier %q is misconfigured", name),
				Description: "Sends resolving hub secrets through this notifier (lifecycle notifications, factory notify actions) will never be delivered. Workflow notify actions, which may override secrets workspace-scoped, are checked separately.",
				OK:          false,
				Error:       err.Error(),
			})
			continue
		}
		checks = append(checks, DoctorCheck{
			Category:    "notifications",
			Severity:    "info",
			Title:       fmt.Sprintf("Notifier %q is configured", name),
			Description: fmt.Sprintf("Type %s constructed successfully with its hub secrets resolved.", nc.Type),
			OK:          true,
		})
	}

	// Lifecycle delivery only uses EffectiveRoutes. Check every one separately
	// so a bad secondary route is visible even when another route is healthy.
	// This intentionally complements (rather than replaces) the full config
	// validation above: malformed configs still get their structural error.
	// Gated on IsEnabled for the same reason ValidateNotificationsConfig is: a
	// muted lifecycle block may legitimately point at a notifier the operator
	// has since deleted ("an operator who mutes alerts and then deletes the
	// notifier must not be left with a hub that refuses to load"), and nothing
	// is delivered through those routes anyway.
	if nCfg.Lifecycle.IsEnabled() {
		for i, route := range nCfg.Lifecycle.EffectiveRoutes() {
			via := strings.TrimSpace(route.Via)
			label := fmt.Sprintf("Lifecycle route %d (%q)", i+1, via)
			nc, ok := nCfg.Notifiers[via]
			if via == "" || !ok {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       label + " names an unknown notifier",
					Description: "This lifecycle route will not deliver messages until its via names a configured notifier.",
				})
				continue
			}
			channel, _ := s.notifierSettings(nc)["channel"].(string)
			if strings.TrimSpace(channel) == "" {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       label + " has no channel",
					Description: fmt.Sprintf("Notifier %q needs a channel for this lifecycle route.", via),
				})
				continue
			}
			if _, err := notify.New(nc.Type, s.notifierSettings(nc), secrets); err != nil {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       label + " is not constructible",
					Description: fmt.Sprintf("Messages for this lifecycle route through notifier %q will not be delivered.", via),
					Error:       err.Error(),
				})
				continue
			}
			checks = append(checks, DoctorCheck{
				Category: "notifications", Severity: "info", OK: true,
				Title:       label + " is configured",
				Description: fmt.Sprintf("Notifier %q constructed successfully and has a channel.", via),
			})
		}
	}

	// Scheduled reports. A disabled schedule delivers nothing, so its report
	// and destination checks are skipped: an operator who pauses a schedule
	// before deleting the notifier (or before upgrading to a build that
	// carries the report) must not be nagged about a slot that never fires.
	// But the tick's whole-block gate (ValidateScheduledNotificationsConfig)
	// judges EVERY entry regardless of Enabled, plus the cross-entry
	// duplicate-id rule ValidateScheduledNotification excludes — so entry
	// validity and id uniqueness are checked on disabled entries too (the very
	// entry pausing every schedule must get its own critical row), and no
	// entry may show a green row while the block is rejected and the tick
	// delivers nothing.
	scheduledBlockErr := types.ValidateScheduledNotificationsConfig(nCfg)
	seenScheduledIDs := make(map[string]int, len(nCfg.Scheduled))
	for i, scheduled := range nCfg.Scheduled {
		label := fmt.Sprintf("Scheduled report %d (%q)", i+1, scheduled.ID)
		duplicateOf, duplicate := seenScheduledIDs[scheduled.ID]
		if !duplicate {
			seenScheduledIDs[scheduled.ID] = i + 1
		}
		if scheduled.Enabled != nil && !*scheduled.Enabled {
			if err := types.ValidateScheduledNotification(nCfg, i); err != nil {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       label + " is invalid",
					Description: "The scheduled notifier pauses every schedule while any entry fails validation, even a disabled one; fix or remove this entry.",
					Error:       err.Error(),
				})
			} else if duplicate {
				checks = append(checks, scheduledDuplicateIDCheck(label, duplicateOf))
			}
			continue
		}
		if !ScheduledReportSupported(scheduled.Report) {
			checks = append(checks, DoctorCheck{
				Category: "notifications", Severity: "critical", OK: false,
				Title: label + " names an unknown report",
				Description: fmt.Sprintf("This hub has no report named %q, so nothing is ever sent for this schedule. Supported reports: %s.",
					scheduled.Report, scheduledReportNamesText()),
			})
			continue
		}
		// Each destination gets the same per-destination checks a lifecycle
		// route gets (channel presence, notifier constructibility): a runtime
		// send through a channel-less notifier fails permanently and silently
		// burns the slot, so doctor must not report the schedule green.
		badVia := false
		for _, via := range scheduled.Via {
			nc, ok := nCfg.Notifiers[strings.TrimSpace(via)]
			if !ok {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       label + " sends to an unknown notifier",
					Description: fmt.Sprintf("Destination %q is not a configured notifier, so this schedule delivers nothing there.", via),
				})
				badVia = true
				continue
			}
			channel, _ := s.notifierSettings(nc)["channel"].(string)
			if strings.TrimSpace(channel) == "" {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       fmt.Sprintf("%s destination %q has no channel", label, via),
					Description: fmt.Sprintf("Notifier %q needs a channel for this schedule's reports to be delivered.", via),
				})
				badVia = true
				continue
			}
			if _, err := notify.New(nc.Type, s.notifierSettings(nc), secrets); err != nil {
				checks = append(checks, DoctorCheck{
					Category: "notifications", Severity: "critical", OK: false,
					Title:       fmt.Sprintf("%s destination %q is not constructible", label, via),
					Description: fmt.Sprintf("Reports for this schedule through notifier %q will not be delivered.", via),
					Error:       err.Error(),
				})
				badVia = true
			}
		}
		if badVia {
			continue
		}
		// The slot fields (at/timezone/weekdays) the destination checks above
		// never touch: an invalid one fails the scheduled tick's validity gate
		// — pausing EVERY schedule, not just this one — so the very entry
		// causing the pause must not get the green row below. Runs after the
		// via checks so their more specific rows keep precedence.
		if err := types.ValidateScheduledNotification(nCfg, i); err != nil {
			checks = append(checks, DoctorCheck{
				Category: "notifications", Severity: "critical", OK: false,
				Title:       label + " is invalid",
				Description: "The scheduled notifier pauses every schedule while any entry fails validation; fix or remove this one.",
				Error:       err.Error(),
			})
			continue
		}
		if duplicate {
			checks = append(checks, scheduledDuplicateIDCheck(label, duplicateOf))
			continue
		}
		if scheduledBlockErr != nil {
			// This entry is healthy, but the tick's gate rejects the whole
			// block: a green "is configured" row would contradict the
			// section-level critical row while nothing is delivered.
			checks = append(checks, DoctorCheck{
				Category: "notifications", Severity: "warning", OK: false,
				Title:       label + " is paused while the scheduled block is invalid",
				Description: "This entry is configured correctly, but the scheduled notifier pauses every schedule until the invalid entry flagged above is fixed.",
			})
			continue
		}
		checks = append(checks, DoctorCheck{
			Category: "notifications", Severity: "info", OK: true,
			Title: label + " is configured",
			Description: fmt.Sprintf("Report %s is sent to %s at %s.",
				scheduled.Report, strings.Join(scheduled.Via, ", "), scheduledSlotDescription(scheduled)),
		})
	}
	return checks
}

// scheduledDuplicateIDCheck is the per-entry row for a schedule reusing an
// earlier entry's id — the one rule ValidateScheduledNotification excludes
// (it needs the whole list), yet one the tick's whole-block gate enforces.
func scheduledDuplicateIDCheck(label string, duplicateOf int) DoctorCheck {
	return DoctorCheck{
		Category: "notifications", Severity: "critical", OK: false,
		Title: label + " duplicates another schedule's id",
		Description: fmt.Sprintf("Schedule ids key delivery state, and the scheduled notifier pauses every schedule while two entries share one (here with scheduled report %d). Give this entry a unique id.",
			duplicateOf),
	}
}

// scheduledSlotDescription renders a schedule's slot for doctor output.
func scheduledSlotDescription(scheduled types.ScheduledNotificationConfig) string {
	timezone := scheduled.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	days := "every day"
	if len(scheduled.Weekdays) > 0 {
		days = strings.Join(scheduled.Weekdays, ", ")
	}
	return fmt.Sprintf("%s %s, %s", scheduled.At, timezone, days)
}

// checkNotifyActions validates every pipeline notify action the way the
// runtime resolves it. It walks the same artifact the pipeline runner parses
// — each workflow's effective pipeline YAML (which covers both stages: and a
// directly authored pipeline_yaml:) plus every factory's pipeline_yaml — so
// no notify surface reaches executeNotifyAction unvalidated. "via" must name
// a configured hub notifier, and that notifier must construct through the
// provider registry (notify.New validates the type, the provider settings and
// the secrets) under the same secret scope a real send would use: workflow
// claws prefer workspace-scoped secrets over hub secrets, factory claws
// resolve hub secrets only (notifyWorkspaceName). Construction verdicts are
// deduped per (notifier, secret scope), and every title names the
// workflow/factory stage so none can collide with checkNotifications', which
// reports the hub-scoped (lifecycle) resolution of the same notifiers.
func (s *Server) checkNotifyActions(cfg *types.HubConfig) []DoctorCheck {
	type notifySource struct {
		kind      string // "Workflow" or "Factory"
		name      string
		yaml      string
		workspace string // secret scope; empty resolves hub secrets only
	}
	var sources []notifySource
	if workspaces, err := loadExternalWorkspaces(); err == nil {
		for _, workspace := range workspaces {
			if workspace == nil {
				continue
			}
			for _, workflow := range workspace.Workflows {
				if workflow == nil || strings.TrimSpace(workflow.PipelineYAML) == "" {
					continue
				}
				sources = append(sources, notifySource{"Workflow", workflow.Name, workflow.PipelineYAML, workspace.Name})
			}
		}
	}
	for _, factory := range s.resolveFactories() {
		if factory == nil || strings.TrimSpace(factory.PipelineYAML) == "" {
			continue
		}
		sources = append(sources, notifySource{"Factory", factory.Name, factory.PipelineYAML, ""})
	}

	var notifiers map[string]types.NotifierConfig
	if cfg.Notifications != nil {
		notifiers = cfg.Notifications.Notifiers
	}

	hubResolver := func(name string) (string, bool) {
		v, ok := cfg.Secrets[name]
		return v, ok
	}
	workspaceSecretsCache := map[string]map[string]string{}
	resolverFor := func(workspace string) notify.SecretResolver {
		if workspace == "" {
			return hubResolver
		}
		secrets, ok := workspaceSecretsCache[workspace]
		if !ok {
			secrets, _ = loadWorkspaceSecrets(workspace)
			workspaceSecretsCache[workspace] = secrets
		}
		return func(name string) (string, bool) {
			if v, ok := secrets[name]; ok {
				return v, true
			}
			return hubResolver(name)
		}
	}

	var checks []DoctorCheck
	notifyActions := 0
	allValid := true
	fail := func(title, description, errText string) {
		allValid = false
		checks = append(checks, DoctorCheck{
			Category:    "notifications",
			Severity:    "critical",
			Title:       title,
			Description: description,
			OK:          false,
			Error:       errText,
		})
	}
	// One construction verdict per (notifier, secret scope): every stage that
	// routes through the same notifier under the same scope shares the row.
	constructed := map[string]bool{}
	for _, src := range sources {
		scopeDesc := "hub secrets"
		if src.workspace != "" {
			scopeDesc = fmt.Sprintf("workspace %q secrets over hub secrets", src.workspace)
		}
		// notifyActionRefs is the same walk the author-time save check
		// (validateNotifyVias, on workflow and factory saves) runs, so the
		// two judge identical refs;
		// an unparseable pipeline is a different failure (the runner logs it)
		// and carries no notify actions to validate. The doctor alone goes on
		// to construct each referenced notifier with real secret resolution —
		// that half must stay on-demand, because settings and secrets rot
		// after a workflow is saved.
		for _, ref := range notifyActionRefs(src.yaml) {
			notifyActions++
			where := fmt.Sprintf("%s %q stage %q", src.kind, src.name, ref.StageID)
			via := ref.Via
			if via == "" {
				fail(fmt.Sprintf("%s notify action has no \"via\"", where),
					fmt.Sprintf("%s has a notify action without a \"via\" notifier name; it can never send and only warns in the claw conversation at runtime.", where), "")
				continue
			}
			nc, ok := notifiers[via]
			if !ok {
				fail(fmt.Sprintf("%s references missing notifier %q", where, via),
					fmt.Sprintf("%s references notifier %q, which is not configured under notifications.notifiers in hub.yaml.", where, via), "")
				continue
			}
			scopeKey := via + "\x00" + src.workspace
			if constructed[scopeKey] {
				continue
			}
			constructed[scopeKey] = true
			if _, err := notify.New(nc.Type, s.notifierSettings(nc), resolverFor(src.workspace)); err != nil {
				fail(fmt.Sprintf("%s notifier %q is misconfigured", where, via),
					fmt.Sprintf("Notify actions through %q will never be delivered: it fails to construct resolving %s.", via, scopeDesc),
					err.Error())
			}
		}
	}

	if notifyActions > 0 && allValid {
		checks = append(checks, DoctorCheck{
			Category:    "notifications",
			Severity:    "info",
			Title:       "Pipeline notify actions configured",
			Description: fmt.Sprintf("%d notify action(s) reference valid notifiers whose secrets resolve in their runtime scope.", notifyActions),
			OK:          true,
		})
	}
	return checks
}

func (s *Server) checkHubSettings(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	factories, _ := loadExternalFactories()
	factoryCount := len(factories)
	if cfg.MaxConcurrentClaws == 1 && factoryCount > 1 {
		checks = append(checks, DoctorCheck{
			Category:    "hub",
			Severity:    "info",
			Title:       "Low concurrency limit with multiple factories",
			Description: fmt.Sprintf("Max concurrent agents is 1 but %d factories are configured. Most factory-created agents will queue as pending.", factoryCount),
			OK:          false,
			FixAction: &FixAction{
				Type:   "navigate",
				Target: "/settings/runtimes",
				Label:  "Increase Limit",
			},
		})
	} else {
		checks = append(checks, DoctorCheck{
			Category:    "hub",
			Severity:    "info",
			Title:       "Hub settings look good",
			Description: fmt.Sprintf("Max concurrent agents is %d with %d factories configured.", cfg.MaxConcurrentClaws, factoryCount),
			OK:          true,
		})
	}

	return checks
}
