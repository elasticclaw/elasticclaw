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
// It has two tiers. The plain tier is a Message that sets nothing beyond
// Text, Target, Thread and Options: providers send Text as a plain message —
// this is what a template-rendered pipeline notify action supplies when it
// renders only a body. Setting any semantic field (Title, Subject, Emoji,
// Severity, Body, Fields, Link, Summary) opts the message into rich
// rendering: those fields describe the notification's meaning — headline,
// severity, subject, detail, metadata — so each provider renders every one of
// them natively (Slack: Block Kit with a colour stripe; an email or Teams
// provider could build its own layout) instead of receiving pre-flattened
// text. Providers must never silently drop a populated semantic field; when
// Title is empty, Text takes its place as the message's leading line.
type Message struct {
	// Text is the plain-text body. When no semantic field is set it is the
	// whole message; when Title is set providers may ignore it; when other
	// semantic fields are set but Title is empty it renders as the leading
	// line of the rich layout.
	Text string
	// Target overrides the notifier's default destination (channel, address).
	Target string
	// Thread is the opaque handle (as returned by an earlier Send) of the
	// message this one is a reply to. Providers that support threading map it
	// onto their native mechanism (Slack: thread_ts); providers without
	// threading ignore it and post top-level.
	Thread string
	// Options is a provider-specific passthrough for anything the fields
	// above do not model. Providers ignore keys they do not understand, and
	// Options can never override the core payload fields a provider computes
	// itself.
	Options map[string]any

	// Subject names what the message is about ("ENG-42 — Fix login bug").
	// Providers with a native subject line (email) can use it there; like
	// every semantic field, setting it opts the message into rich rendering,
	// and providers without a subject slot render it as part of that layout
	// (Slack: its own line) rather than dropping it.
	Subject string

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

// Semantic reports whether any semantic-tier field is set, i.e. whether the
// message opted into rich rendering. Providers use it to pick the rendering
// tier so a populated semantic field is never silently dropped.
func (m Message) Semantic() bool {
	return m.Title != "" || m.Subject != "" || m.Emoji != "" || m.Severity != "" ||
		m.Body != "" || len(m.Fields) > 0 || m.Link != (Link{}) || len(m.Summary) > 0
}

// Notifier sends messages to one configured destination.
//
// Send returns an opaque provider-specific handle for the delivered message
// (Slack: the message ts), or "" for providers without message handles. The
// handle exists so a caller can relate a follow-up message to an earlier one
// by placing the earlier handle in Message.Thread; a provider without
// threading returns "" and ignores Thread, degrading gracefully.
type Notifier interface {
	Send(ctx context.Context, msg Message) (handle string, err error)
}

// DestinationReporter is implemented by providers that can name their
// configured default destination (Slack: the channel ID). Callers use it only
// as opaque bookkeeping — e.g. thread roots recorded for one destination must
// stop matching after the operator repoints the notifier elsewhere. Callers
// must treat a provider without this interface (or an empty string) as an
// unknown destination, never as a match-everything wildcard.
type DestinationReporter interface {
	Destination() string
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

// ClassifiedError is implemented by provider errors that know their class.
// Providers should classify every failure they can recognize — either by
// implementing this interface on their own error types or by wrapping with
// ConfigError/PermanentError — because unclassified errors default to
// transient and callers will retry them indefinitely. There is deliberately
// no TransientError wrapper: transient is already the default for any
// unclassified error, so the wrapper would have nothing to add.
type ClassifiedError interface {
	NotifyErrorClass() ErrorClass
}

// Classify reports the ErrorClass of a Send failure. Errors that carry no
// classification are treated as transient, the safe default: they are
// retried instead of being dropped.
func Classify(err error) ErrorClass {
	var ce ClassifiedError
	if errors.As(err, &ce) {
		return ce.NotifyErrorClass()
	}
	return ErrorTransient
}

// classedError attaches an ErrorClass to an arbitrary error.
type classedError struct {
	err   error
	class ErrorClass
}

func (e *classedError) Error() string                { return e.err.Error() }
func (e *classedError) Unwrap() error                { return e.err }
func (e *classedError) NotifyErrorClass() ErrorClass { return e.class }

// ConfigError marks err as a notifier-configuration failure: every message
// fails alike until the operator fixes the config, so callers pause delivery.
func ConfigError(err error) error {
	if err == nil {
		return nil
	}
	return &classedError{err: err, class: ErrorConfig}
}

// PermanentError marks err as specific to this message and unrecoverable by
// retry (malformed or oversized payload, an invalid per-message target):
// callers record it and move on.
func PermanentError(err error) error {
	if err == nil {
		return nil
	}
	return &classedError{err: err, class: ErrorPermanent}
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
