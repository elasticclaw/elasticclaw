package aichat

import (
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

func (d Deps) ticketCreator(workspace string, cfg *config.Config) (tracker.Creator, tracker.Target, []string) {
	if cfg.IssueTracker == nil || d.LinearToken == nil {
		return nil, tracker.Target{}, nil
	}
	token, ok := d.LinearToken(workspace)
	if !ok || token == "" {
		return nil, tracker.Target{}, nil
	}
	fields := cfg.IssueTracker.DefaultFields
	return tracker.NewLinear(fields.Team, token, d.HTTPClient), tracker.LinearTarget(fields.Team, fields.State, fields.Labels), []string{token}
}

func (d Deps) availableActions(workspace string, cfg *config.Config) []string {
	actions := []string{"artifact"}
	if creator, _, _ := d.ticketCreator(workspace, cfg); creator != nil {
		actions = append(actions, "ticket")
	}
	return actions
}
