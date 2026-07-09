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
			writeErr(w, http.StatusBadRequest, "bad_request", "name required")
			return
		}
		s.handleMCPDelete(w, r, name)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
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
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}
	if req.Source == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "source required")
		return
	}
	switch types.MCPSource(req.Source) {
	case types.MCPSourceNpx, types.MCPSourceUvx, types.MCPSourceSmithery:
		if req.Package == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "package required for source "+req.Source)
			return
		}
	case types.MCPSourceDocker:
		if req.Image == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "image required for source docker")
			return
		}
	case types.MCPSourceSSE:
		if req.URL == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "url required for source sse")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid source: must be npx, uvx, smithery, docker, or sse")
		return
	}

	s.cfgMu.Lock()
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
		s.cfgMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "internal", "failed to save config: "+err.Error())
		return
	}
	s.hubCfg = &cfgCopy
	s.cfgMu.Unlock()

	jsonOK(w, map[string]string{"upserted": req.Name})
}

func (s *Server) handleMCPDelete(w http.ResponseWriter, _ *http.Request, name string) {
	s.cfgMu.Lock()
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
		s.cfgMu.Unlock()
		writeErr(w, http.StatusNotFound, "not_found", "mcp server not found")
		return
	}
	cfgCopy.MCPServers = mcps
	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		s.cfgMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "internal", "failed to save config: "+err.Error())
		return
	}
	s.hubCfg = &cfgCopy
	s.cfgMu.Unlock()

	jsonOK(w, map[string]string{"deleted": name})
}
