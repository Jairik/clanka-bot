// Package notify delivers Notifications through ntfy. It owns the live
// notify.url setting, the optional ntfy access token, and the randomly
// generated topic used by the public ntfy.sh default.
//
// ntfy.sh topics are public in the sense that anyone who knows the topic can
// subscribe; the random topic is an unguessable name, not access control.
// Official ntfy access tokens are issued by the server (or provisioned in a
// self-hosted server's auth-tokens). A locally invented bearer token cannot
// authenticate ntfy, so this package never fabricates one: it generates the
// topic once, stores it in the write-only secrets table, and uses NTFY_TOKEN
// only when the user has stored a real server-issued token.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Settings keys shared with the Dashboard and the notify Tool.
const (
	// URLKey is the ntfy server base URL. It defaults to the public ntfy.sh.
	URLKey = "notify.url"
	// MissedAlertsKey controls whether a missed Timer or Alarm is pushed.
	MissedAlertsKey = "notify.missed_alerts"
)

// Secret names. The topic is generated on first use; the token is optional and
// must be created by the ntfy server. Both live only in the secrets table.
const (
	// TopicSecret holds the point-to-point ntfy topic.
	TopicSecret = "NTFY_TOPIC"
	// TokenSecret holds an optional server-issued ntfy access token.
	TokenSecret = "NTFY_TOKEN"
)

const (
	// DefaultURL is the public ntfy server.
	DefaultURL = "https://ntfy.sh"
	// DefaultMissedAlerts pushes a Notification when a Ring reaches max_ring or
	// a recovered missed row is found.
	DefaultMissedAlerts = true
	// topicPrefix makes a generated topic recognisable without shortening its
	// random suffix.
	topicPrefix = "clanka-"
	// requestTimeout bounds one publish, including connect and response.
	requestTimeout = 10 * time.Second
	// maxResponseBytes bounds the discarded response body so a hostile or
	// broken server cannot grow memory.
	maxResponseBytes = 4 << 10
)

// ErrEmptyMessage reports a publish with nothing to say.
var ErrEmptyMessage = errors.New("notify: message is required")

// ErrMuted is the sentinel a Quiet-hours suppression unwraps to.
var ErrMuted = errors.New("notify: notifications are muted")

// MutedError reports a Notification suppressed by Quiet hours. The caller may
// explain the reason to the user; nothing was sent and nothing is retried.
type MutedError struct {
	// Reason is "quiet hours" or "do not disturb".
	Reason string
}

func (e *MutedError) Error() string {
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "quiet hours"
	}
	return "notify: notifications are muted during " + reason
}

// Unwrap makes errors.Is(err, ErrMuted) work.
func (e *MutedError) Unwrap() error { return ErrMuted }

// ErrPublish is the sentinel an HTTP failure unwraps to.
var ErrPublish = errors.New("notify: publish failed")

// HTTPError reports a non-2xx ntfy response. The response body is deliberately
// not retained: it can echo request details and is never useful to the user.
type HTTPError struct {
	// StatusCode is the HTTP status ntfy returned.
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("notify: ntfy returned HTTP %d", e.StatusCode)
}

// Unwrap makes errors.Is(err, ErrPublish) work.
func (e *HTTPError) Unwrap() error { return ErrPublish }

// Config is the config.yaml layer of the Notify settings.
type Config struct {
	// URL is the ntfy server base URL.
	URL string `yaml:"url"`
	// MissedAlerts pushes a Notification for a missed Timer or Alarm.
	MissedAlerts bool `yaml:"missed_alerts"`
}

// DefaultConfig is the shipped Notify configuration.
func DefaultConfig() Config {
	return Config{URL: DefaultURL, MissedAlerts: DefaultMissedAlerts}
}

// Validate checks the config layer before startup uses it.
func (c Config) Validate() error {
	_, err := normalizeBaseURL(c.URL)
	return err
}

// Definitions builds the live Notify settings for the one settings service.
func Definitions(cfg Config) []settings.Definition {
	if strings.TrimSpace(cfg.URL) == "" {
		cfg.URL = DefaultURL
	}
	return []settings.Definition{
		settings.String(URLKey, DefaultURL,
			settings.WithConfig(cfg.URL),
			settings.WithValidator(validURL),
			settings.WithDescription("ntfy server base URL. It defaults to https://ntfy.sh; a self-hosted server may be used. The topic and optional token are secrets, never settings."),
		),
		settings.Bool(MissedAlertsKey, DefaultMissedAlerts,
			settings.WithConfig(cfg.MissedAlerts),
			settings.WithDescription("Send a phone Notification when a Timer or Alarm reaches max_ring without an answer, or a missed alert is recovered at startup."),
		),
	}
}

// validURL rejects a stored or Dashboard value that is not a usable base URL.
func validURL(value any) error {
	text, ok := value.(string)
	if !ok {
		return errors.New("notify: url must be a string")
	}
	_, err := normalizeBaseURL(text)
	return err
}

// Reader is the settings read seam. *settings.Service satisfies it.
type Reader interface {
	Get(key string) (settings.Value, bool)
}

// SecretStore is the write-only secret seam. *secrets.Service satisfies it.
type SecretStore interface {
	Get(name string) (string, bool)
	Set(ctx context.Context, name, value string) (secrets.Metadata, error)
}

// Notification is one ntfy message.
type Notification struct {
	// Kind is a log label such as "phone" or "missed_timer". It is never sent.
	Kind string
	// Title is the notification title; empty uses ntfy's topic title.
	Title string
	// Message is the plain-text body.
	Message string
	// Priority is an ntfy priority such as "default", "high" or "max".
	Priority string
	// Tags are ntfy tags or emoji short codes.
	Tags []string
}

// MissedAlert is one Scheduled alert that reached the missed state. A Timer or
// Alarm manager reports the durable fact; the Notifier decides whether, where
// and how to push it.
type MissedAlert struct {
	// Kind is "timer" or "alarm".
	Kind string
	// Label is the user's short name for the alert, if any.
	Label string
	// Reason is "max_ring", "restart" or "downtime".
	Reason string
	// Duration is the alert's configured length.
	Duration time.Duration
	// FiresAt, FiredAt and EndedAt are the durable history times.
	FiresAt time.Time
	FiredAt time.Time
	EndedAt time.Time
}

// Options configures a Service. Reader, Quiet and Secrets are optional so the
// package can be constructed before the live settings service exists.
type Options struct {
	Reader     Reader
	Quiet      *quiet.Service
	Secrets    SecretStore
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Now and Rand are injectable for deterministic tests. Rand is only used
	// to generate the topic on first use.
	Rand io.Reader
	// Config is the effective fallback when no Reader is bound or the key is
	// absent, so startup recovery can use the config layer.
	Config Config
}

// Service publishes Notifications and provisions the topic once. It is safe
// for concurrent use.
type Service struct {
	mu     sync.RWMutex
	reader Reader
	quiet  *quiet.Service
	// topicMu serialises the one-time topic generation. Without it two
	// concurrent first sends could each generate and store a different topic.
	topicMu sync.Mutex

	secrets SecretStore
	client  *http.Client
	logger  *slog.Logger
	rand    io.Reader
	config  Config
}

// New builds a Notify service. The config layer is validated here so a bad URL
// fails startup instead of a late Notification.
func New(opts Options) (*Service, error) {
	cfg := opts.Config
	if strings.TrimSpace(cfg.URL) == "" {
		cfg.URL = DefaultURL
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: requestTimeout}
	if opts.HTTPClient != nil {
		client = *opts.HTTPClient
		if client.Timeout <= 0 {
			client.Timeout = requestTimeout
		}
	}
	// A publish is a single request. Refusing redirects keeps the bearer token
	// on the configured host and turns a redirect into a visible HTTP error.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	random := opts.Rand
	if random == nil {
		random = rand.Reader
	}
	return &Service{
		reader:  opts.Reader,
		quiet:   opts.Quiet,
		secrets: opts.Secrets,
		client:  &client,
		logger:  logger,
		rand:    random,
		config:  cfg,
	}, nil
}

// Bind attaches or replaces the live settings reader. main binds a startup
// reader for recovered missed alerts and rebinds the full service after Tool
// registration.
func (s *Service) Bind(reader Reader) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.reader = reader
	s.mu.Unlock()
}

// Send publishes one Notification. Nothing is sent during Quiet hours; the
// caller gets a MutedError and no retry is scheduled. The topic is provisioned
// only after the Quiet gate, so a muted Notification has no network or storage
// side effect.
func (s *Service) Send(ctx context.Context, n Notification) error {
	if s == nil {
		return errors.New("notify: service is not configured")
	}
	if strings.TrimSpace(n.Message) == "" {
		return ErrEmptyMessage
	}
	if s.quiet != nil {
		if allowed, reason := s.quiet.AllowDelivery(); !allowed {
			s.logger.Info("notification muted", "kind", n.Kind, "reason", reason)
			return &MutedError{Reason: reason}
		}
	}
	base, err := normalizeBaseURL(s.urlValue())
	if err != nil {
		return err
	}
	topic, err := s.Topic(ctx)
	if err != nil {
		return err
	}
	payload := publishRequest{
		Topic:    topic,
		Message:  n.Message,
		Title:    strings.TrimSpace(n.Title),
		Priority: strings.TrimSpace(n.Priority),
		Tags:     n.Tags,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: encode message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "clanka-bot")
	if token, ok := s.token(); ok {
		// The token is a server-issued credential. It is never logged, never
		// put in the URL and never included in an error.
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: publish: %w", err)
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the response is
	// not logged because a server could echo request data.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	s.logger.Info("notification sent", "kind", n.Kind, "status", resp.StatusCode)
	return nil
}

// Topic returns the stored ntfy topic, generating and storing it on first use.
// A topic generated once stays stable across restarts and concurrent callers.
// An existing but invalid value is an error, never silently overwritten.
func (s *Service) Topic(ctx context.Context) (string, error) {
	if s == nil || s.secrets == nil {
		return "", errors.New("notify: secrets store is not available")
	}
	if value, ok := s.secrets.Get(TopicSecret); ok {
		if validTopic(value) {
			return value, nil
		}
		return "", errors.New("notify: NTFY_TOPIC must be 1-64 letters, digits, underscores or dashes")
	}
	// Only one first-use caller may generate. The second re-reads the stored
	// value inside the lock and never creates a second topic.
	s.topicMu.Lock()
	defer s.topicMu.Unlock()
	if value, ok := s.secrets.Get(TopicSecret); ok {
		if validTopic(value) {
			return value, nil
		}
		return "", errors.New("notify: NTFY_TOPIC must be 1-64 letters, digits, underscores or dashes")
	}
	topic, err := generateTopic(s.rand)
	if err != nil {
		return "", fmt.Errorf("notify: generate topic: %w", err)
	}
	if _, err := s.secrets.Set(ctx, TopicSecret, topic); err != nil {
		return "", fmt.Errorf("notify: store topic: %w", err)
	}
	return topic, nil
}

// NotifyMissed delivers a missed-Notification when the setting is enabled and
// Quiet hours allow it. It runs from the Timer/Alarm recovery and max_ring
// hooks; failures are logged and never panic the alert path.
func (s *Service) NotifyMissed(ctx context.Context, alert MissedAlert) {
	if s == nil {
		return
	}
	if !s.missedAlertsEnabled() {
		s.logger.Debug("missed notification disabled", "kind", alert.Kind)
		return
	}
	if s.quiet != nil {
		if allowed, reason := s.quiet.AllowDelivery(); !allowed {
			// Muted means dropped, matching the Quiet-hours spec. Do not
			// schedule a retry when the window ends.
			s.logger.Info("missed notification muted", "kind", alert.Kind, "reason", reason)
			return
		}
	}
	if err := s.Send(ctx, missedNotification(alert)); err != nil {
		s.logger.Warn("missed notification failed",
			"kind", alert.Kind,
			"reason", alert.Reason,
			"error", err.Error())
	}
}

// urlValue resolves the live base URL, falling back to the config layer.
func (s *Service) urlValue() string {
	s.mu.RLock()
	reader := s.reader
	s.mu.RUnlock()
	if reader != nil {
		if value, ok := reader.Get(URLKey); ok {
			if text, ok := value.Value.(string); ok {
				return text
			}
		}
	}
	return s.config.URL
}

// missedAlertsEnabled resolves the live missed-alert switch.
func (s *Service) missedAlertsEnabled() bool {
	s.mu.RLock()
	reader := s.reader
	s.mu.RUnlock()
	if reader != nil {
		if value, ok := reader.Get(MissedAlertsKey); ok {
			if enabled, ok := value.Value.(bool); ok {
				return enabled
			}
		}
	}
	return s.config.MissedAlerts
}

// token returns a stored server-issued access token, if any. It never invents
// one: ntfy rejects tokens that the server did not issue.
func (s *Service) token() (string, bool) {
	if s == nil || s.secrets == nil {
		return "", false
	}
	value, ok := s.secrets.Get(TokenSecret)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

// publishRequest is the ntfy JSON publish body. The topic is in the body so a
// topic can never be mangled by path escaping.
type publishRequest struct {
	Topic    string   `json:"topic"`
	Message  string   `json:"message"`
	Title    string   `json:"title,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// missedNotification renders one missed Timer or Alarm. Only the label and
// timing facts cross to the phone; never a Tool result or secret.
func missedNotification(alert MissedAlert) Notification {
	kind := strings.TrimSpace(alert.Kind)
	if kind == "" {
		kind = "alert"
	}
	subject := "Your " + kind
	if label := strings.TrimSpace(alert.Label); label != "" {
		subject += " \"" + label + "\""
	}
	body := subject + " was missed."
	switch alert.Reason {
	case "restart":
		body = subject + " was still ringing when the assistant restarted, so it was missed."
	case "downtime":
		body = subject + " was missed while the assistant was offline."
	default:
		if alert.Duration > 0 {
			body = fmt.Sprintf("%s was missed after %s.", subject, alert.Duration.Round(time.Second))
		}
	}
	return Notification{
		Kind:     "missed_" + kind,
		Title:    "Missed " + kind,
		Message:  body,
		Priority: "high",
		Tags:     []string{"warning"},
	}
}

// normalizeBaseURL validates and canonicalises the ntfy base URL. Credentials,
// query strings and fragments are rejected so a token cannot hide in the URL
// and leak into logs or a redirect.
func normalizeBaseURL(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return DefaultURL, nil
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return "", errors.New("notify: url must be a valid http or https URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("notify: url must use http or https")
	}
	parsed.Scheme = scheme
	if parsed.Host == "" {
		return "", errors.New("notify: url must include a host")
	}
	if parsed.User != nil {
		return "", errors.New("notify: url must not contain credentials; store a token in " + TokenSecret)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("notify: url must not contain a query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// validTopic checks the ntfy topic character set and length.
func validTopic(topic string) bool {
	if len(topic) == 0 || len(topic) > 64 {
		return false
	}
	for _, r := range topic {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// generateTopic makes a 128-bit random topic, the unguessable-name default for
// the public ntfy.sh server. It is not access control; see the package doc.
func generateTopic(random io.Reader) (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", err
	}
	return topicPrefix + hex.EncodeToString(buf), nil
}
