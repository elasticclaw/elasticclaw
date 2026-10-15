package tracker

import "context"

// Creator is separate from the read registry. Only a user-initiated action calls it.
type Creator interface {
	Target(context.Context) (Target, error)
	Create(context.Context, Draft) (Issue, error)
}

type Target struct {
	Tracker  string   `json:"tracker"`
	Verb     string   `json:"verb"`
	Provider string   `json:"-"`
	Team     string   `json:"team"`
	State    string   `json:"state"`
	Labels   []string `json:"labels"`
}

type Draft struct {
	Title, Body, Team, State string
	Labels                   []string
	// BeforeSend durably marks a potentially committed write before sending it.
	// If it fails, the provider must not send the mutation.
	BeforeSend func() error
}

type Issue struct {
	Identifier, URL, Title, State, Team string
	Labels                              []string
}

// CreateError contains only safe, actionable text, never a provider response.
type CreateError struct {
	Message    string
	Validation bool
}

func (e *CreateError) Error() string { return e.Message }

const LinearUnconfirmed = "Linear did not confirm the ticket. Check Linear before trying again."
const LinearRejected = "Linear did not accept the ticket."

func LinearTarget(team, state string, labels []string) Target {
	return Target{Tracker: "Linear", Verb: "Open Linear ticket", Provider: "linear", Team: team, State: state, Labels: labels}
}
