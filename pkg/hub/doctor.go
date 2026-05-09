package hub

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// DoctorCheck is a single diagnostic check result.
type DoctorCheck struct {
	Category    string     `json:"category"`              // "auth", "models", "sandboxes", "factories", "integrations", "mcp", "templates"
	Severity    string     `json:"severity"`              // "critical", "warning", "info"
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

	// --- Hub Settings ---
	checks = append(checks, s.checkHubSettings(hubCfg)...)

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
			Description: "At least one LLM key is required for claws to function.",
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
		if key.APIKey == "" {
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
		checks = append(checks, DoctorCheck{
			Category:    "models",
			Severity:    "info",
			Title:       "No default model configured",
			Description: "No default model is set. The first available LLM key will be used, but an explicit default is recommended.",
			OK:          false,
			FixAction: &FixAction{
				Type:   "navigate",
				Target: "/settings/models",
				Label:  "Set Default Model",
			},
		})
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
			Description: "At least one sandbox provider (daytona, vercel, replicated, local) must be configured.",
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
		"daytona": true, "vercel": true, "replicated": true, "local": true,
	}

	allProvidersValid := true
	for name, p := range cfg.Providers {
		if !validProviders[name] {
			allProvidersValid = false
			checks = append(checks, DoctorCheck{
				Category:    "sandboxes",
				Severity:    "warning",
				Title:       fmt.Sprintf("Unknown sandbox provider: %q", name),
				Description: fmt.Sprintf("Provider %q is not a recognised sandbox provider (daytona, vercel, replicated, local).", name),
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
		case "vercel":
			if p.AccessToken == "" {
				allProvidersValid = false
				checks = append(checks, DoctorCheck{
					Category:    "sandboxes",
					Severity:    "critical",
					Title:       "Vercel provider missing access token",
					Description: "The Vercel sandbox provider is configured but the access token is empty.",
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/runtimes",
						Label:  "Configure Vercel",
					},
				})
			}
		case "local":
			// Local provider doesn't need credentials
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

	if len(cfg.Factories) == 0 {
		checks = append(checks, DoctorCheck{
			Category:    "factories",
			Severity:    "info",
			Title:       "No factories configured",
			Description: "No factories are configured. Factory-based claw creation is disabled.",
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

	for _, f := range cfg.Factories {
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

		// Check 4: overlapping triggers
		var key triggerKey
		switch f.Integration {
		case "linear", "shortcut", "github-issues":
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
				Description: fmt.Sprintf("Factories %s all trigger on the same event (integration=%s, workspace=%s, trigger=%s). This will create duplicate claws.", strings.Join(names, ", "), key.integration, key.workspace, key.trigger),
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
			Description: fmt.Sprintf("%d factory(ies) configured with no issues detected.", len(cfg.Factories)),
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
					Description: fmt.Sprintf("Template %q does not contain an elasticclaw-config.yaml file. This may cause issues when creating claws.", name),
					OK:          false,
					FixAction: &FixAction{
						Type:   "navigate",
						Target: "/settings/templates",
						Label:  "Manage Templates",
					},
				})
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
	for _, f := range cfg.Factories {
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

	if len(cfg.Integrations.Linear) == 0 && len(cfg.Integrations.Shortcut) == 0 && len(cfg.Integrations.GitHubIssues) == 0 {
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

func (s *Server) checkHubSettings(cfg *types.HubConfig) []DoctorCheck {
	var checks []DoctorCheck

	if cfg.MaxConcurrentClaws == 1 && len(cfg.Factories) > 1 {
		checks = append(checks, DoctorCheck{
			Category:    "hub",
			Severity:    "info",
			Title:       "Low concurrency limit with multiple factories",
			Description: fmt.Sprintf("Max concurrent claws is 1 but %d factories are configured. Most factory-created claws will queue as pending.", len(cfg.Factories)),
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
			Description: fmt.Sprintf("Max concurrent claws is %d with %d factories configured.", cfg.MaxConcurrentClaws, len(cfg.Factories)),
			OK:          true,
		})
	}

	return checks
}
