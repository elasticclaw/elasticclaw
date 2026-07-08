// Embedded web UI serving and branding/config endpoints.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

func (s *Server) handleBranding(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var appName, logoURL string
	if s.hubCfg.Branding != nil {
		appName = s.hubCfg.Branding.AppName
		logoURL = s.hubCfg.Branding.LogoURL
	}
	s.mu.RUnlock()
	jsonOK(w, map[string]string{
		"appName": appName,
		"logoUrl": logoURL,
	})
}

func (s *Server) serveWebUI(mux *http.ServeMux, staticFS fs.FS) {
	// Register MIME types that may not be set on the host OS
	// (important for embedded static files served from Go)
	for ext, mimeType := range map[string]string{
		".js":    "application/javascript",
		".mjs":   "application/javascript",
		".css":   "text/css",
		".html":  "text/html",
		".json":  "application/json",
		".svg":   "image/svg+xml",
		".png":   "image/png",
		".ico":   "image/x-icon",
		".woff2": "font/woff2",
		".woff":  "font/woff",
	} {
		mime.AddExtensionType(ext, mimeType)
	}
	// Log what's in the embedded FS for debugging
	if entries, err2 := fs.ReadDir(staticFS, "."); err2 == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		logf("[webui] embedded files: %v", names)
	}

	// Wrap file server to serve index.html for directory requests
	// (needed for Next.js static export with trailingSlash: true)
	serveFile := func(w http.ResponseWriter, r *http.Request, path string) {
		content, err := fs.ReadFile(staticFS, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ext := filepath.Ext(path)
		if ct, ok := map[string]string{
			".html": "text/html; charset=utf-8",
			".js":   "application/javascript",
			".css":  "text/css",
			".json": "application/json",
			".svg":  "image/svg+xml",
			".png":  "image/png",
			".ico":  "image/x-icon",
			".txt":  "text/plain",
		}[ext]; ok {
			w.Header().Set("Content-Type", ct)
		}
		w.Write(content)
	}

	fileServer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		// Try exact path first (file or directory)
		if f, err := staticFS.Open(p); err == nil {
			stat, _ := f.Stat()
			f.Close()
			if stat != nil && !stat.IsDir() {
				serveFile(w, r, p)
				return
			}
			// It's a dir — try index.html inside
			serveFile(w, r, strings.TrimRight(p, "/")+"/index.html")
			return
		}
		// embed.FS doesn't support Open() on directories — the dir check above
		// may have failed. Try index.html at this path before falling back.
		if !strings.HasSuffix(p, "/index.html") {
			idxPath := strings.TrimRight(p, "/") + "/index.html"
			if f, err := staticFS.Open(idxPath); err == nil {
				f.Close()
				serveFile(w, r, idxPath)
				return
			}
		}
		if workspacePath, ok := settingsWorkspaceStaticPath(p); ok {
			if f, err := staticFS.Open(workspacePath); err == nil {
				f.Close()
				serveFile(w, r, workspacePath)
				return
			}
		}
		// Unknown path — serve root index.html (SPA fallback)
		serveFile(w, r, "index.html")
	})

	// Serve static files openly — auth is enforced client-side (sessionStorage)
	// and on the API endpoints (withAuth middleware).
	// Static HTML/JS/CSS files don't contain secrets so no server-side gate needed.
	mux.Handle("/", fileServer)
}

func settingsWorkspaceStaticPath(requestPath string) (string, bool) {
	p := strings.Trim(strings.TrimPrefix(requestPath, "/"), "/")
	if p == "" {
		return "", false
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "settings" {
		return "", false
	}
	if parts[1] == "" || settingsStaticSection(parts[1]) || strings.HasPrefix(parts[1], "_") {
		return "", false
	}
	if len(parts) == 2 {
		return "settings/_workspace/index.html", true
	}
	if !settingsStaticSection(parts[2]) {
		return "", false
	}
	return "settings/_workspace/" + parts[2] + "/index.html", true
}

func settingsStaticSection(section string) bool {
	switch section {
	case "runtimes",
		"models",
		"github",
		"authentication",
		"issue-trackers",
		"workspaces",
		"workflows",
		"workspace-analytics",
		"secrets",
		"ai-config",
		"mcp-servers",
		"analytics",
		"doctor",
		"troubleshoot":
		return true
	default:
		return false
	}
}

func (s *Server) handleHubConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	hubURL := s.hubCfg.URL
	if s.hubCfg.PublicURL != "" {
		hubURL = s.hubCfg.PublicURL
	}
	token := s.hubCfg.Token
	var appName, logoURL string
	if s.hubCfg.Branding != nil {
		appName = s.hubCfg.Branding.AppName
		logoURL = s.hubCfg.Branding.LogoURL
	}
	s.mu.RUnlock()
	if hubURL == "" {
		hubURL = "http://localhost:8080"
	}
	jsonOK(w, map[string]interface{}{
		"token":   token,
		"hubUrl":  hubURL,
		"version": Version,
		"appName": appName,
		"logoUrl": logoURL,
	})
}
