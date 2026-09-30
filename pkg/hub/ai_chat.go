package hub

import (
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat"
)

func (s *Server) aiChatDeps() aichat.Deps {
	return aichat.Deps{
		WebAuth:     s.withWebAuth,
		WithFeature: s.withFeature,
		CallerLogin: func(r *http.Request) string { return aichat.OwnerLogin(githubLoginFromContext(r.Context())) },
		Workspaces:  listExternalWorkspaceNames,
		ManagedDir:  workspaceManagedDir,
	}
}
