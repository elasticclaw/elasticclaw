// Package notify provides outbound notifications from the hub to external
// services (Slack today; email, Google Chat, Teams, etc. later). Transport
// configuration lives in hub-level named notifiers; callers reference a
// notifier by name and supply a provider-agnostic Message. Adding a provider
// is one new file plus one registry entry.
package notify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Severity classifies a message's outcome so each provider can map it onto
// its own convention (Slack: attachment colour stripe; email could use a
// subject tag). Providers must never require it: an empty Severity renders a
// neutral message.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeveritySuccess Severity = "success"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Field is one short metadata pair rendered dimly after the main content,
// e.g. {"repo", "acme/app"}. Label may be empty for self-describing values
// ("workflow bugfix"). Code hints that the value is a machine identifier;
// providers with rich text may render it in monospace, others ignore it.
type Field struct {
	Label string
	Value string
	Code  bool
}

// Link is the message's primary subject link (a PR, an issue). When Label is
// empty the link decorates the Subject; when both URL and Label are set the
// link is its own line and the Subject renders separately.
type Link struct {
	URL   string
	Label string
}

// Message is the provider-agnostic notification payload.
//
// It has two tiers. The plain tier (Text, Subject, Target, Options) is the
// lowest common denominator: when Title is empty, providers send Text as a
// plain message — this is what template-rendered pipeline notify actions
// supply. The semantic tier (Title and friends) describes the notification's
// meaning — headline, severity, subject, detail, metadata — so each provider
// renders it natively (Slack: Block Kit with a colour stripe; an email or
// Teams provider could build its own layout) instead of receiving
// pre-flattened text.
type Message struct {
	// Text is the plain-text body. When Title is empty it is the whole
	// message; when Title is set providers may ignore it.
	Text string
	// Subject names what the message is about ("ENG-42 — Fix login bug").
	// Providers with a native subject line (email) can use it there.
	Subject string
	// Target overrides the notifier's default destination (channel, address).
	Target string
	// Options is a provider-specific passthrough (e.g. Slack thread_ts).
	// Providers ignore keys they do not understand, and Options can never
	// override the core payload fields a provider computes itself.
	Options map[string]any

	// Title is the human headline ("Agent crashed during startup"). Setting
	// it opts the message into rich rendering.
	Title string
	// Emoji is a semantic hint in Slack shortcode form (":boom:"). Providers
	// without emoji support must ignore it and still render a good message.
	Emoji string
	// Severity maps to provider-native emphasis (colour, tags).
	Severity Severity
	// Body is the long detail (failure reason, quoted diagnostics). It may be
	// very long; providers clamp it to their own limits.
	Body string
	// Fields are dim metadata pairs appended after the main content.
	Fields []Field
	// Link is the primary subject link when one exists.
	Link Link
	// Summary holds short, ordered context fragments ("acme/app", "ENG-42",
	// a compressed failure reason) that providers join into one-line
	// renderings such as push-notification text. Empty entries are dropped.
	Summary []string
}

// Notifier sends messages to one configured destination.
//
// Send returns an opaque provider-specific handle for the delivered message
// (Slack: the message ts), or "" for providers without message handles. The
// handle exists so a caller can relate follow-up messages to an earlier one
// through provider Options (e.g. Slack threading via Options["thread_ts"]);
// the interface itself carries no thread concept because most transports
// (email, webhooks) have none. A provider without threading returns "" and
// ignores unknown Options, degrading gracefully.
type Notifier interface {
	Send(ctx context.Context, msg Message) (handle string, err error)
}

// PayloadRenderer is implemented by providers that can render the exact wire
// payload for a Message without sending it. It backs dry-run previews: the
// same rendering path Send uses, so a preview is always exactly what a real
// send would post.
type PayloadRenderer interface {
	RenderPayload(msg Message) (map[string]any, error)
}

// SecretResolver resolves a hub secret by name.
type SecretResolver func(name string) (string, bool)

// Constructor builds a Notifier from its configuration. Secrets are resolved
// at construction time so misconfiguration fails fast.
type Constructor func(cfg map[string]any, secrets SecretResolver) (Notifier, error)

// ErrorClass buckets Send failures for the caller's retry policy.
type ErrorClass int

const (
	// ErrorTransient failures may succeed on retry (network blips, rate
	// limits, provider 5xx). Unknown errors classify as transient.
	ErrorTransient ErrorClass = iota
	// ErrorPermanent failures are specific to this message and never succeed
	// on retry (oversized or malformed payload): record and move on.
	ErrorPermanent
	// ErrorConfig failures mean the notifier configuration is broken (bad
	// token, missing channel) and every message fails alike: callers should
	// pause delivery rather than burn each message as failed.
	ErrorConfig
)

// classifiedError is implemented by provider errors that know their class.
type classifiedError interface {
	NotifyErrorClass() ErrorClass
}

// Classify reports the ErrorClass of a Send failure. Errors that carry no
// classification are treated as transient, the safe default: they are
// retried instead of being dropped.
func Classify(err error) ErrorClass {
	var ce classifiedError
	if errors.As(err, &ce) {
		return ce.NotifyErrorClass()
	}
	return ErrorTransient
}

// provider bundles a constructor with the metadata the doctor needs to
// validate a notifier config without constructing it. New provider types add
// one entry here, next to their implementation file, and doctor coverage is
// inherited automatically.
type provider struct {
	construct Constructor
	// secretSettings lists the config keys whose values must name an
	// existing hub secret (e.g. "token_secret" for slack).
	secretSettings []string
}

var registry = map[string]provider{
	"slack": {construct: newSlack, secretSettings: []string{"token_secret"}},
}

// New builds a Notifier of the given type from its configuration.
func New(typ string, cfg map[string]any, secrets SecretResolver) (Notifier, error) {
	p, ok := registry[typ]
	if !ok {
		return nil, fmt.Errorf("unknown notifier type %q (supported: %s)", typ, supportedTypes())
	}
	return p.construct(cfg, secrets)
}

// SecretSettings returns the config keys of the given notifier type whose
// values must reference an existing secret. Returns nil for unknown types.
func SecretSettings(typ string) []string {
	p, ok := registry[typ]
	if !ok {
		return nil
	}
	return p.secretSettings
}

// Supported reports whether the given notifier type is registered.
func Supported(typ string) bool {
	_, ok := registry[typ]
	return ok
}

func supportedTypes() string {
	types := make([]string, 0, len(registry))
	for typ := range registry {
		types = append(types, typ)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

// stringOption reads an optional string key from a notifier config map.
func stringOption(cfg map[string]any, key string) string {
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}
