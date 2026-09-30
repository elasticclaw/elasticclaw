// Package config loads the workspace AI Chat configuration without resolving secrets.
package config

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config describes <workspace managed directory>/ai_chat.yaml. About and each
// modes.{explore_idea,validate_hypothesis,ask_the_data,write_brief}.prompt are
// optional text. LLM optionally names an entry in hub.yaml's llm_keys.
// Repositories is a list of GitHub owner/repo names. KnowledgeBase describes a
// GitHub repo, optional branch, and entry file. PostHog and Datadog credentials
// are secret names (never literal values), resolved later from workspace then
// global secrets. IssueTracker selects Linear and its default team/state/labels.
// Retention defaults to 90/30/30 days. Reserved conventions, template, skill,
// knowledge_base.write and knowledge_base.save fields are accepted and ignored.
// Missing/malformed files invalidate the workspace; individual bad sources are
// removed from Config and retained as invalid Source entries for the UI.
type Config struct {
	About         string          `yaml:"about"`
	Modes         map[string]Mode `yaml:"modes"`
	LLM           string          `yaml:"llm"`
	KnowledgeBase *KnowledgeBase  `yaml:"-"`
	Repositories  []string        `yaml:"-"`
	PostHog       *PostHog        `yaml:"-"`
	Datadog       *Datadog        `yaml:"-"`
	IssueTracker  *IssueTracker   `yaml:"-"`
	Retention     Retention       `yaml:"retention"`
	Sources       []Source        `yaml:"-"`
}

type Mode struct {
	Prompt string `yaml:"prompt"`
}
type KnowledgeBase struct {
	Provider string `yaml:"provider"`
	Repo     string `yaml:"repo"`
	Branch   string `yaml:"branch"`
	Entry    string `yaml:"entry"`
}
type PostHog struct {
	Host      string `yaml:"host"`
	ProjectID string `yaml:"project_id"`
	APIKey    string `yaml:"api_key"`
}
type Datadog struct {
	Site   string `yaml:"site"`
	Env    string `yaml:"env"`
	APIKey string `yaml:"api_key"`
	AppKey string `yaml:"app_key"`
}
type IssueTracker struct {
	Provider      string `yaml:"provider"`
	DefaultFields struct {
		Team   string   `yaml:"team"`
		State  string   `yaml:"state"`
		Labels []string `yaml:"labels"`
	} `yaml:"default_fields"`
}
type Retention struct {
	ChatsDays            int `yaml:"chats_days"`
	ToolResultsDays      int `yaml:"tool_results_days"`
	ArtifactVersionsDays int `yaml:"artifact_versions_days"`
}
type Source struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var hostnameLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// Load reads at call time. managedDir must come from the hub workspace resolver.
func Load(managedDir string) (*Config, error) {
	f, err := os.Open(filepath.Join(managedDir, "ai_chat.yaml"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var root yaml.Node
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("invalid ai_chat.yaml")
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ai_chat.yaml must be a mapping")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("ai_chat.yaml must contain one document")
	}
	cfg := &Config{Retention: Retention{90, 30, 30}, Sources: []Source{}}
	if err := root.Decode(cfg); err != nil {
		return nil, fmt.Errorf("invalid ai_chat.yaml fields")
	}
	if cfg.Retention.ChatsDays <= 0 || cfg.Retention.ToolResultsDays <= 0 || cfg.Retention.ArtifactVersionsDays <= 0 {
		return nil, fmt.Errorf("retention days must be positive")
	}
	for mode := range cfg.Modes {
		switch mode {
		case "explore_idea", "validate_hypothesis", "ask_the_data", "write_brief":
		default:
			return nil, fmt.Errorf("unsupported mode")
		}
	}
	if cfg.LLM != "" && strings.TrimSpace(cfg.LLM) == "" {
		return nil, fmt.Errorf("llm must name an llm_keys entry")
	}
	var nodes map[string]yaml.Node
	if err := root.Decode(&nodes); err != nil {
		return nil, fmt.Errorf("invalid ai_chat.yaml fields")
	}
	// Decode sources independently so a malformed source cannot disable the chat.
	cfg.decodeSource(nodes, "knowledge_base", "Knowledge base", func(n yaml.Node) bool {
		var v KnowledgeBase
		if n.Decode(&v) != nil || v.Provider != "github" || !validRepo(v.Repo) || !validEntry(v.Entry) {
			return false
		}
		cfg.KnowledgeBase = &v
		return true
	}, "Expected provider github, repo owner/repo and a relative entry file")
	if n, ok := nodes["repositories"]; ok {
		source := Source{Kind: "repositories", Name: "Repositories", Status: "unchecked"}
		if n.Kind != yaml.SequenceNode {
			source.Status = "invalid"
			source.Error = "Expected a list of GitHub owner/repo names"
		} else {
			var rejected []string
			for i, item := range n.Content {
				var repo string
				if item.Decode(&repo) != nil || !validRepo(repo) {
					if item.Kind == yaml.ScalarNode && item.Value != "" {
						rejected = append(rejected, item.Value)
					} else {
						rejected = append(rejected, fmt.Sprintf("entry %d", i+1))
					}
				} else {
					cfg.Repositories = append(cfg.Repositories, repo)
				}
			}
			if len(rejected) > 0 {
				source.Status = "invalid"
				source.Error = "Rejected: " + strings.Join(rejected, ", ") + " - expected GitHub owner/repo names"
			}
		}
		cfg.Sources = append(cfg.Sources, source)
	}
	cfg.decodeSource(nodes, "posthog", "PostHog", func(n yaml.Node) bool {
		var v PostHog
		if n.Decode(&v) != nil || !validHost(v.Host) || strings.TrimSpace(v.ProjectID) == "" || !validSecret(v.APIKey) {
			return false
		}
		cfg.PostHog = &v
		return true
	}, "Expected an HTTP(S) host, project_id and api_key secret name")
	cfg.decodeSource(nodes, "datadog", "Datadog", func(n yaml.Node) bool {
		var v Datadog
		if n.Decode(&v) != nil || !validDatadogSite(v.Site) || strings.TrimSpace(v.Env) == "" || !validSecret(v.APIKey) || !validSecret(v.AppKey) {
			return false
		}
		cfg.Datadog = &v
		return true
	}, "Expected a Datadog site, env and api_key/app_key secret names")
	cfg.decodeSource(nodes, "issue_tracker", "Linear", func(n yaml.Node) bool {
		var v IssueTracker
		if n.Decode(&v) != nil || v.Provider != "linear" || strings.TrimSpace(v.DefaultFields.Team) == "" {
			return false
		}
		cfg.IssueTracker = &v
		return true
	}, "Expected provider linear and default_fields.team")
	return cfg, nil
}

func (c *Config) decodeSource(nodes map[string]yaml.Node, kind, name string, decode func(yaml.Node) bool, message string) {
	n, ok := nodes[kind]
	if !ok {
		return
	}
	source := Source{Kind: kind, Name: name, Status: "unchecked"}
	if n.Kind != yaml.MappingNode || !decode(n) {
		source.Status = "invalid"
		source.Error = message
	}
	c.Sources = append(c.Sources, source)
}

func validRepo(repo string) bool {
	if !repoPattern.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
func validEntry(entry string) bool {
	return entry != "" && entry != "." && !path.IsAbs(entry) && path.Clean(entry) == entry && entry != ".." && !strings.HasPrefix(entry, "../") && !strings.ContainsAny(entry, "\\\x00")
}
func validSecret(ref string) bool { return strings.TrimSpace(ref) != "" }
func validHost(host string) bool {
	u, err := url.Parse(host)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func validDatadogSite(site string) bool {
	if len(site) > 253 || !strings.Contains(site, ".") {
		return false
	}
	for _, label := range strings.Split(site, ".") {
		if !hostnameLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}
