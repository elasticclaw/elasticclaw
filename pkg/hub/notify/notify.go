// Package notify provides outbound messaging from workflow pipelines to
// external services (Slack today; email, Google Chat, Teams, etc. later).
// Transport configuration lives in workspace-level notifiers; pipelines only
// reference a notifier by name and supply the message.
package notify

import (
	"context"
	"fmt"
)

// Message is the provider-agnostic payload. Text is the lowest common
// denominator; Subject is used by providers that have one (email), Target
// optionally overrides the notifier's default destination, and Options is a
// provider-specific passthrough (e.g. Slack thread_ts or blocks).
type Message struct {
	Text    string
	Subject string
	Target  string
	Options map[string]any
}

// Notifier sends messages to one configured destination.
type Notifier interface {
	Send(ctx context.Context, msg Message) error
}

// SecretResolver resolves a hub secret by name.
type SecretResolver func(name string) (string, bool)

// Constructor builds a Notifier from its workspace configuration. Secrets are
// resolved at construction time so misconfiguration fails fast.
type Constructor func(cfg map[string]any, secrets SecretResolver) (Notifier, error)

var registry = map[string]Constructor{
	"slack": newSlack,
}

// New builds a Notifier of the given type from its configuration.
func New(typ string, cfg map[string]any, secrets SecretResolver) (Notifier, error) {
	ctor, ok := registry[typ]
	if !ok {
		return nil, fmt.Errorf("unknown notifier type %q (supported: %s)", typ, supportedTypes())
	}
	return ctor(cfg, secrets)
}

// Supported reports whether the given notifier type is registered.
func Supported(typ string) bool {
	_, ok := registry[typ]
	return ok
}

func supportedTypes() string {
	out := ""
	for typ := range registry {
		if out != "" {
			out += ", "
		}
		out += typ
	}
	return out
}

// stringOption reads an optional string key from a notifier config map.
func stringOption(cfg map[string]any, key string) string {
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}
