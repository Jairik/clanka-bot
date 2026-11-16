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
	"strconv"
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
	// RemindersKey controls whether a due Reminder is pushed to the phone.
	RemindersKey = "notify.reminders"
	// CooldownKey is the minimum gap between two missed-alert summaries.
	CooldownKey = "notify.cooldown"
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
	// DefaultReminders pushes a Notification when a Reminder is due.
	DefaultReminders = true
	// DefaultCooldown is the shipped gap between two missed-alert summaries.
	DefaultCooldown = time.Minute
	// recoveryHold is the debounce window for recovered missed alerts: each
	// arrival re-arms it, so a pass that keeps reporting stays one batch and
	// flushes recoveryHold after the last alert. Timer and Alarm report their
	// recovered rows in one synchronous pass, so the whole burst normally joins
	// one summary, while a lone recovered alert reaches the phone almost at
	// once.
	recoveryHold = 2 * time.Second
	// maxSummaryNames bounds the names listed in one summary. The alert count
	// stays exact; the tail becomes "and N more".
	maxSummaryNames = 6
	// topicPrefix makes a generated topic recognisable without shortening its
	// random suffix.
	topicPrefix = "clanka-"
	// requestTimeout bounds one publish, including connect and response.
	requestTimeout = 10 * time.Second
	// FlushTimeout bounds Close's synchronous drain. A single publish is also
	// capped by requestTimeout; this is the ceiling callers use for the whole
	// drain so shutdown cannot hang.
	FlushTimeout = requestTimeout
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
	// Reminders pushes a Notification when a Reminder is due.
	Reminders bool `yaml:"reminders"`
}

// DefaultConfig is the shipped Notify configuration.
func DefaultConfig() Config {
	return Config{URL: DefaultURL, MissedAlerts: DefaultMissedAlerts, Reminders: DefaultReminders}
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
		settings.Bool(RemindersKey, DefaultReminders,
			settings.WithConfig(cfg.Reminders),
			settings.WithDescription("Send a phone Notification when a Reminder is due, alongside the spoken reminder. Quiet hours mute it."),
		),
		settings.Duration(CooldownKey, DefaultCooldown,
			settings.WithValidator(validCooldown),
			settings.WithDescription("Minimum gap between two phone Notifications for missed Timers and Alarms. Startup recovery sends one summary for the whole burst; a second recovery inside this gap is held until the gap ends instead of being dropped, so no alert is lost. A live max_ring alert is never delayed."),
		),
	}
}

// validCooldown rejects a stored or Dashboard cooldown that cannot be a gap.
func validCooldown(value any) error {
	cooldown, ok := value.(time.Duration)
	if !ok {
		return errors.New("notify: cooldown must be a duration")
	}
	if cooldown < 0 {
		return errors.New("notify: cooldown must not be negative")
	}
	return nil
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
	// Priority is an ntfy priority name ("min", "low", "default", "high",
	// "max"/"urgent") or the number 1-5. JSON publishing requires the numeric
	// form; Send maps the name before encoding.
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
	// Now is the clock the missed-alert cooldown compares against. Zero uses
	// time.Now.
	Now func() time.Time
	// Rand is injectable for deterministic tests. It is only used to generate
	// the topic on first use.
	Rand io.Reader
	// RecoveryHold overrides the debounce window for recovered missed alerts so
	// one recovery pass becomes one Notification. Zero uses recoveryHold, and a
	// negative value disables the hold so each alert is pushed at once.
	RecoveryHold time.Duration
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
	now     func() time.Time
	hold    time.Duration
	config  Config

	// pushMu serialises missed-alert summaries so two flushes cannot both pass
	// the cooldown check. lastSummary is written under it.
	pushMu      sync.Mutex
	lastSummary time.Time

	// batchMu guards the held recovery batch, its one timer and the closed
	// flag. timerAt is the real-world time the timer is set to fire, so a
	// debounce hold and a cooldown gap can both be honoured when re-arming.
	batchMu      sync.Mutex
	pending      []MissedAlert
	pendingTimer *time.Timer
	timerAt      time.Time
	closed       bool
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
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	hold := recoveryHold
	switch {
	case opts.RecoveryHold < 0:
		hold = 0
	case opts.RecoveryHold > 0:
		hold = opts.RecoveryHold
	}
	return &Service{
		reader:  opts.Reader,
		quiet:   opts.Quiet,
		secrets: opts.Secrets,
		client:  &client,
		logger:  logger,
		rand:    random,
		now:     now,
		hold:    hold,
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
// side effect. Send is never rate limited: a dictated notify.phone message and
// a due Reminder each send exactly one request, and notify.cooldown only
// bounds the missed-alert summaries.
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
	priority, err := priorityValue(n.Priority)
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
		Priority: priority,
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
		value = strings.TrimSpace(value)
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
		value = strings.TrimSpace(value)
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
//
// A live alert (a Ring that just reached max_ring) is pushed at once: it is one
// fact about one alert and the user may be standing next to the device. A
// recovery pass reports every open and downtime-retired row in a burst, so
// those alerts are debounced for recoveryHold and pushed as one summary naming
// them instead of one push per row (#206). A summary blocked by notify.cooldown
// is held until the gap ends rather than dropped.
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
	s.batchMu.Lock()
	closed := s.closed
	s.batchMu.Unlock()
	if isRecoveryReason(alert.Reason) && s.hold > 0 && !closed {
		s.queueRecovery(alert)
		return
	}
	// A live alert, or a recovery after Close, cannot wait for a timer that
	// will never flush it, so it is delivered at once.
	s.deliver(ctx, missedNotification(alert))
}

// isRecoveryReason reports whether a missed alert came from the startup
// recovery pass rather than a Ring that just reached max_ring. Recovery is the
// burst: one pass reports every open and downtime-retired row.
func isRecoveryReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "restart", "downtime":
		return true
	}
	return false
}

// queueRecovery holds one recovered alert until the recovery pass settles. The
// hold is a debounce: every arrival pushes the timer out, so a pass that keeps
// reporting stays one batch and flushes s.hold after the *last* alert rather
// than s.hold after the first.
func (s *Service) queueRecovery(alert MissedAlert) {
	s.batchMu.Lock()
	s.pending = append(s.pending, alert)
	s.armTimerLocked(time.Now().Add(s.hold))
	waiting := len(s.pending)
	s.batchMu.Unlock()
	s.logger.Debug("missed alert held for one summary",
		"kind", alert.Kind, "reason", alert.Reason, "waiting", waiting)
}

// armTimerLocked schedules the recovery flush for the later of at and any
// timer already running; batchMu must be held. Keeping the later deadline lets
// a debounce hold and a cooldown remainder compose instead of one cutting the
// other short.
func (s *Service) armTimerLocked(at time.Time) {
	if s.pendingTimer != nil && s.timerAt.After(at) {
		at = s.timerAt
	}
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
	}
	delay := time.Until(at)
	if delay < 0 {
		delay = 0
	}
	s.timerAt = at
	s.pendingTimer = time.AfterFunc(delay, s.flushAfterHold)
}

// flushAfterHold runs on the recovery timer. The recovery pass's context is
// gone by then, so the push uses a fresh one; the HTTP client still bounds it.
func (s *Service) flushAfterHold() {
	_ = s.flushPending(context.Background())
}

// flushPending pushes the held recovery alerts as one Notification. A push
// blocked by notify.cooldown returns a retry delay: the batch is kept and the
// timer re-armed for the remainder of the gap, so the rate limit still bounds
// the phone but no recovered alert is lost. It returns the publish error for a
// caller that wants it; the timer path only logs.
func (s *Service) flushPending(ctx context.Context) error {
	s.batchMu.Lock()
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
		s.pendingTimer = nil
	}
	s.timerAt = time.Time{}
	batch := s.pending
	s.pending = nil
	s.batchMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	retry, err := s.pushSummary(ctx, batch, false)
	if retry > 0 {
		// Cooldown deferred the push. Keep the whole batch plus anything that
		// arrived while the cooldown was being checked, and try again when the
		// gap ends.
		s.batchMu.Lock()
		s.pending = append(batch, s.pending...)
		s.armTimerLocked(time.Now().Add(retry))
		s.batchMu.Unlock()
	}
	return err
}

// pushSummary sends one Notification covering a whole recovery batch. It
// returns a positive retry delay when notify.cooldown blocked the push and the
// batch should be re-queued; zero means the batch was delivered or permanently
// suppressed (a mute is dropped, matching the Quiet-hours rule) and must not be
// retried. force bypasses the cooldown for Close's final drain: there is no
// later flush, so an alert held at shutdown must go out now.
func (s *Service) pushSummary(ctx context.Context, batch []MissedAlert, force bool) (time.Duration, error) {
	cooldown := s.cooldown()
	now := s.now()
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if !force && cooldown > 0 && !s.lastSummary.IsZero() {
		if elapsed := now.Sub(s.lastSummary); elapsed < cooldown {
			retry := cooldown - elapsed
			s.logger.Debug("missed notification deferred by cooldown",
				"alerts", len(batch), "retry_ms", retry.Milliseconds())
			// A sub-millisecond remainder would re-arm in a tight loop before
			// the clock can advance; one millisecond settles it.
			if retry < time.Millisecond {
				retry = time.Millisecond
			}
			return retry, nil
		}
	}
	err := s.Send(ctx, missedSummaryNotification(batch))
	if err != nil {
		var muted *MutedError
		if errors.As(err, &muted) {
			s.logger.Info("missed notification muted", "alerts", len(batch), "reason", muted.Reason)
			return 0, nil
		}
		s.logger.Warn("missed notification failed", "alerts", len(batch), "error", err.Error())
		return 0, err
	}
	s.lastSummary = now
	return 0, nil
}

// Close stops the recovery timer and drains any held batch synchronously. It is
// safe on a nil service and safe to call more than once. A cooldown does not
// suppress the final drain, because a held alert would otherwise be lost for
// good: the next boot sees only terminal history rows. The caller supplies the
// deadline; a single publish is also bounded by the HTTP client's timeout.
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.batchMu.Lock()
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
		s.pendingTimer = nil
	}
	s.timerAt = time.Time{}
	batch := s.pending
	s.pending = nil
	s.closed = true
	s.batchMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	_, err := s.pushSummary(ctx, batch, true)
	return err
}

// deliver publishes one missed Notification, logging a failure or a mute. The
// alert path never sees a notification error.
func (s *Service) deliver(ctx context.Context, n Notification) {
	if err := s.Send(ctx, n); err != nil {
		var muted *MutedError
		if errors.As(err, &muted) {
			s.logger.Info("missed notification muted", "kind", n.Kind, "reason", muted.Reason)
			return
		}
		s.logger.Warn("missed notification failed", "kind", n.Kind, "error", err.Error())
	}
}

// NotifyReminder pushes one due Reminder to the phone when the setting is on
// and Quiet hours allow it. The message is user content, so it is never logged;
// a failure logs only the kind and the error. A Reminder is never batched and
// never cooldown-suppressed: the user asked for it, so each one is one request.
func (s *Service) NotifyReminder(ctx context.Context, message string) {
	if s == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if !s.remindersEnabled() {
		s.logger.Debug("reminder notification disabled")
		return
	}
	err := s.Send(ctx, Notification{
		Kind:     "reminder",
		Title:    "Reminder",
		Message:  message,
		Priority: "high",
		Tags:     []string{"bell"},
	})
	if err == nil {
		return
	}
	var muted *MutedError
	if errors.As(err, &muted) {
		// Muted means dropped, matching the Quiet-hours spec.
		s.logger.Info("reminder notification muted", "reason", muted.Reason)
		return
	}
	s.logger.Warn("reminder notification failed", "error", err.Error())
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

// remindersEnabled resolves the live due-Reminder switch.
func (s *Service) remindersEnabled() bool {
	s.mu.RLock()
	reader := s.reader
	s.mu.RUnlock()
	if reader != nil {
		if value, ok := reader.Get(RemindersKey); ok {
			if enabled, ok := value.Value.(bool); ok {
				return enabled
			}
		}
	}
	return s.config.Reminders
}

// cooldown resolves the live gap between two missed-alert summaries. A missing
// or unusable value falls back to the shipped default, so a hand-edited read of
// the wrong type can never silence or flood the phone.
func (s *Service) cooldown() time.Duration {
	s.mu.RLock()
	reader := s.reader
	s.mu.RUnlock()
	if reader != nil {
		if value, ok := reader.Get(CooldownKey); ok {
			if cooldown, ok := value.Value.(time.Duration); ok && cooldown >= 0 {
				return cooldown
			}
		}
	}
	return DefaultCooldown
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
// topic can never be mangled by path escaping, and the priority is numeric
// because ntfy's JSON API accepts 1-5, not the header names.
type publishRequest struct {
	Topic    string   `json:"topic"`
	Message  string   `json:"message"`
	Title    string   `json:"title,omitempty"`
	Priority *int     `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// priorityValue maps a Notification priority name to the numeric form the JSON
// API requires. An empty value uses ntfy's default.
func priorityValue(raw string) (*int, error) {
	text := strings.ToLower(strings.TrimSpace(raw))
	if text == "" {
		return nil, nil
	}
	value := 0
	switch text {
	case "min":
		value = 1
	case "low":
		value = 2
	case "default":
		value = 3
	case "high":
		value = 4
	case "max", "urgent":
		value = 5
	default:
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > 5 {
			return nil, errors.New("notify: priority must be min, low, default, high, max or 1-5")
		}
		value = parsed
	}
	return &value, nil
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

// missedSummaryNotification renders one recovery batch as a single
// Notification. Only the same two fields the single-alert message already
// sends cross to the phone: the kind and the user's label. Never a Tool result,
// a Reminder message or a secret. A batch of one keeps the detailed wording.
func missedSummaryNotification(batch []MissedAlert) Notification {
	if len(batch) == 1 {
		return missedNotification(batch[0])
	}
	names := make([]string, 0, len(batch))
	for _, alert := range batch {
		names = append(names, alertName(alert))
	}
	listed := names
	tail := ""
	if len(listed) > maxSummaryNames {
		listed = listed[:maxSummaryNames]
		tail = fmt.Sprintf(" and %d more", len(names)-maxSummaryNames)
	}
	message := fmt.Sprintf("You missed %d alerts: %s%s.", len(names), strings.Join(listed, ", "), tail)
	return Notification{
		Kind:     summaryKind(batch),
		Title:    "Missed alerts",
		Message:  message,
		Priority: "high",
		Tags:     []string{"warning"},
	}
}

// alertName is the short name a summary uses for one alert: the label plus its
// kind ("pasta timer"), or just the kind when it has no label. A label is
// user-supplied, so it is flattened to one line instead of allowing the
// summary to grow extra lines.
func alertName(alert MissedAlert) string {
	kind := strings.TrimSpace(alert.Kind)
	if kind == "" {
		kind = "alert"
	}
	label := strings.Join(strings.Fields(alert.Label), " ")
	if label == "" {
		return kind
	}
	return label + " " + kind
}

// summaryKind is the log label for a batch: one kind when the whole batch is
// that kind, otherwise the mixed "missed". It is never sent.
func summaryKind(batch []MissedAlert) string {
	kind := ""
	for _, alert := range batch {
		next := strings.TrimSpace(alert.Kind)
		if next == "" {
			continue
		}
		if kind != "" && kind != next {
			return "missed"
		}
		kind = next
	}
	if kind == "" {
		return "missed"
	}
	return "missed_" + kind
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
