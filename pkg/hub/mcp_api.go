package hub

import (
	"encoding/json"
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// handleMCPCrud handles:
//
//	GET    /api/mcp         → list MCP servers (redacted)
//	PUT    /api/mcp         → upsert an MCP server
//	DELETE /api/mcp?name=x  → delete an MCP server by name
func (s *Server) handleMCPCrud(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMCPList(w, r)
	case http.MethodPut:
		s.handleMCPUpsert(w, r)
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		s.handleMCPDelete(w, r, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPList(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.LoadHubConfig()
	if err != nil || cfg == nil {
		jsonOK(w, map[string][]MCPView{"mcpServers": {}})
		return
	}
	views := make([]MCPView, 0, len(cfg.MCPServers))
	for _, mcp := range cfg.MCPServers {
		if mcp == nil {
			continue
		}
		view := MCPView{
			Name:    mcp.Name,
			Source:  string(mcp.Source),
			Package: mcp.Package,
			Image:   mcp.Image,
			URL:     mcp.URL,
			Enabled: mcp.Enabled,
			Config:  mcp.Config,
			Command: mcp.Command,
		}
		if mcp.Secrets != nil {
			for envVar := range mcp.Secrets {
				view.Secrets = append(view.Secrets, envVar)
			}
		}
		views = append(views, view)
	}
	jsonOK(w, map[string][]MCPView{"mcpServers": views})
}

type mcpUpsertRequest struct {
	Name    string            `json:"name"`
	Source  string            `json:"source"`
	Package string            `json:"package,omitempty"`
	Image   string            `json:"image,omitempty"`
	URL     string            `json:"url,omitempty"`
	Enabled bool              `json:"enabled"`
	Config  map[string]string `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	Command []string          `json:"command,omitempty"`
}

func (s *Server) handleMCPUpsert(w http.ResponseWriter, r *http.Request) {
	var req mcpUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if req.Source == "" {
		http.Error(w, "source required", http.StatusBadRequest)
		return
	}
	switch types.MCPSource(req.Source) {
	case types.MCPSourceNpx, types.MCPSourceUvx, types.MCPSourceSmithery:
		if req.Package == "" {
			http.Error(w, "package required for source "+req.Source, http.StatusBadRequest)
			return
		}
	case types.MCPSourceDocker:
		if req.Image == "" {
			http.Error(w, "image required for source docker", http.StatusBadRequest)
			return
		}
	case types.MCPSourceSSE:
		if req.URL == "" {
			http.Error(w, "url required for source sse", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "invalid source: must be npx, uvx, smithery, docker, or sse", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	cfgCopy := *s.hubCfg

	// Preserve existing secrets if the request omits them (partial update)
	if req.Secrets == nil {
		for _, existing := range cfgCopy.MCPServers {
			if existing.Name == req.Name && existing.Secrets != nil {
				req.Secrets = existing.Secrets
				break
			}
		}
	}

	var mcps []*types.MCPServerHubConfig
	found := false
	for _, existing := range cfgCopy.MCPServers {
		if existing.Name == req.Name {
			// Update existing
			mcps = append(mcps, &types.MCPServerHubConfig{
				Name:    req.Name,
				Source:  types.MCPSource(req.Source),
				Package: req.Package,
				Image:   req.Image,
				URL:     req.URL,
				Enabled: req.Enabled,
				Config:  req.Config,
				Secrets: req.Secrets,
				Command: req.Command,
			})
			found = true
		} else {
			mcps = append(mcps, existing)
		}
	}
	if !found {
		mcps = append(mcps, &types.MCPServerHubConfig{
			Name:    req.Name,
			Source:  types.MCPSource(req.Source),
			Package: req.Package,
			Image:   req.Image,
			URL:     req.URL,
			Enabled: req.Enabled,
			Config:  req.Config,
			Secrets: req.Secrets,
			Command: req.Command,
		})
	}
	cfgCopy.MCPServers = mcps
	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		s.mu.Unlock()
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.hubCfg = &cfgCopy
	s.mu.Unlock()

	jsonOK(w, map[string]string{"upserted": req.Name})
}

func (s *Server) handleMCPDelete(w http.ResponseWriter, _ *http.Request, name string) {
	s.mu.Lock()
	cfgCopy := *s.hubCfg
	var mcps []*types.MCPServerHubConfig
	found := false
	for _, existing := range cfgCopy.MCPServers {
		if existing.Name == name {
			found = true
			continue
		}
		mcps = append(mcps, existing)
	}
	if !found {
		s.mu.Unlock()
		http.Error(w, "mcp server not found", http.StatusNotFound)
		return
	}
	cfgCopy.MCPServers = mcps
	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		s.mu.Unlock()
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.hubCfg = &cfgCopy
	s.mu.Unlock()

	jsonOK(w, map[string]string{"deleted": name})
}
