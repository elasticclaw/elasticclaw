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
		Workspaces: func() ([]string, error) {
			workspaces, err := loadExternalWorkspaces()
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(workspaces))
			for _, workspace := range workspaces {
				names = append(names, workspace.Name)
			}
			return names, nil
		},
		ManagedDir: workspaceManagedDir,
	}
}
