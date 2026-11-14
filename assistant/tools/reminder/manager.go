package reminder

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
)

// Kind is how a Reminder's due time was set. It changes how List and Speak
// describe the time, not how it fires.
type Kind string

const (
	// KindDuration is a Reminder set as "in 20 minutes".
	KindDuration Kind = "duration"
	// KindClock is a Reminder set as "at 5 pm" in the effective timezone.
	KindClock Kind = "clock"
)

// overdueGrace is the downtime rule shared with Timers: a Reminder overdue by
// this much or less speaks at startup; an older one joins one summary.
const overdueGrace = 2 * time.Minute

// storeTimeout bounds the startup read and the small writes. The database is
// local, so a slow query means it is wedged, not busy.
const storeTimeout = 5 * time.Second

// Reminder is one scheduled message.
type Reminder struct {
	ID               string    `json:"id"`
	Message          string    `json:"message"`
	Kind             Kind      `json:"kind"`
	DurationSeconds  int       `json:"duration_seconds,omitempty"`
	FiresAt          time.Time `json:"fires_at"`
	RemainingSeconds int       `json:"remaining_seconds"`
	// When is the spoken form of the due time at the moment this value was
	// read: "in 20 minutes", "at 5:00 pm" or "tomorrow at 7:00 am".
	When string `json:"when"`
}

// entry is one live Reminder and its callback.
type entry struct {
	reminder Reminder
	stop     *time.Timer
}

// CuePlayer is the Reminder Cue seam. *sound.Service satisfies it.
type CuePlayer interface {
	Clip(role sound.Role) (sound.Clip, bool)
	PlayClip(ctx context.Context, clip sound.Clip) error
}

// QuietGate is the Quiet-hours playback seam. *quiet.Service satisfies it: the
// Cue is scaled and the message is spoken at quiet.volume, while an urgent
// queue item keeps the alert out of the held Announcement summary.
type QuietGate interface {
	Clip(clip sound.Clip) sound.Clip
	Speak(ctx context.Context, speaker quiet.Speaker, text string) error
}

// Options are the shared seams a Manager uses. The zero value is usable: the
// clock is the real one, the zone is UTC and speech is unavailable.
type Options struct {
	Now     func() time.Time
	Logger  *slog.Logger
	Zone    *tz.Service
	Speaker announce.Speaker
	Queue   *announce.Queue
	Cues    CuePlayer
	Quiet   QuietGate
	// Notify pushes a due Reminder to the phone. Nil delivers nothing; a
	// failure never blocks or repeats the spoken reminder.
	Notify Notifier
}

// Notifier is the one Notification seam the Manager needs. tool.Notifier
// satisfies it.
type Notifier interface {
	NotifyReminder(ctx context.Context, message string)
}

// Manager owns the live Reminders. Without a store it is the in-memory
// implementation used by tests and the conformance suite; AttachStore adds
// durability and the downtime rule.
type Manager struct {
	mu      sync.Mutex
	now     func() time.Time
	logger  *slog.Logger
	zone    *tz.Service
	speaker announce.Speaker
	queue   *announce.Queue
	cues    CuePlayer
	quiet   QuietGate
	notify  Notifier

	db      *sql.DB
	history *alertstore.Store
	closed  bool

	// wg tracks the synchronous part of a fire (the row/history transaction and
	// queue enqueue) so Close can finish before the database closes.
	wg      sync.WaitGroup
	seq     int
	entries map[string]*entry

	// active holds one cancellable playback context per fired Reminder, keyed by
	// the id its history row uses, so a cancel can stop a message that is
	// already speaking or waiting in the Announcement queue.
	active    map[string]context.CancelFunc
	cancelled map[string]bool
	// activeMessages keeps each fired Reminder's message. The pending row is
	// gone before playback, so a query cancel ("cancel the oven reminder") needs
	// this metadata to match a message that is speaking or queued.
	activeMessages map[string]string
}

// NewManager builds an empty manager.
func NewManager(opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Manager{
		now:            opts.Now,
		logger:         opts.Logger,
		zone:           opts.Zone,
		speaker:        opts.Speaker,
		queue:          opts.Queue,
		cues:           opts.Cues,
		quiet:          opts.Quiet,
		notify:         opts.Notify,
		entries:        make(map[string]*entry),
		active:         make(map[string]context.CancelFunc),
		activeMessages: make(map[string]string),
		cancelled:      make(map[string]bool),
	}
}

// location reads the effective timezone at scheduling time. A nil zone is UTC;
// a Dashboard or voice change therefore applies to the next clock-time
// Reminder without a restart.
func (m *Manager) location() *time.Location {
	if m.zone == nil {
		return time.UTC
	}
	return m.zone.Location()
}

// StartDuration schedules a Reminder that speaks after d.
func (m *Manager) StartDuration(message string, d time.Duration) (Reminder, error) {
	if d <= 0 {
		return Reminder{}, &Error{Message: "How long should the reminder be?"}
	}
	now := m.now()
	return m.add(Reminder{
		Message:         message,
		Kind:            KindDuration,
		DurationSeconds: int(d.Round(time.Second) / time.Second),
		FiresAt:         now.Add(d),
	}, d)
}

// StartClock schedules a Reminder at a local clock time. A time already passed
// is the next day, matching the Alarm policy; tomorrow is explicit when the
// utterance said so.
func (m *Manager) StartClock(message string, hour, minute int, tomorrow bool) (Reminder, error) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return Reminder{}, &Error{Message: "I couldn't understand that time."}
	}
	dayOffset := 0
	if tomorrow {
		dayOffset = 1
	}
	now := m.now()
	firesAt := nextClockTime(now, hour, minute, dayOffset, m.location())
	return m.add(Reminder{
		Message: message,
		Kind:    KindClock,
		FiresAt: firesAt,
	}, firesAt.Sub(now))
}

// add validates, persists and schedules one Reminder. The caller holds no lock.
func (m *Manager) add(r Reminder, delay time.Duration) (Reminder, error) {
	r.Message = strings.TrimSpace(r.Message)
	if r.Message == "" {
		return Reminder{}, &Error{Message: "What should I remind you about?"}
	}
	if delay < 0 {
		delay = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Reminder{}, errors.New("reminder: manager is closed")
	}
	now := m.now()
	r.RemainingSeconds = remainingSeconds(r.FiresAt, now)
	r.When = whenText(r, now, m.location())

	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		id, err := m.insert(ctx, r, now)
		if err != nil {
			return Reminder{}, err
		}
		r.ID = id
	} else {
		m.seq++
		r.ID = strconv.Itoa(m.seq)
	}

	id := r.ID
	m.entries[id] = &entry{
		reminder: r,
		stop:     time.AfterFunc(delay, func() { m.runFire(id) }),
	}
	// Deliberately no message or duration in the log: the text can be personal.
	m.logger.Info("reminder set", "reminder_id", id, "kind", string(r.Kind), "fires_at", r.FiresAt)
	return r, nil
}

// Error is a spoken validation failure from the manager.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

// runFire registers one callback with Close's wait group, so shutdown can wait
// for the durable fire transaction before the database closes.
func (m *Manager) runFire(id string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	m.fire(id)
}

// fire removes the pending row and opens its history row in one transaction,
// then queues the Cue and message as one alert item. The row is gone before
// the speaker starts, so a restart can never say the same Reminder twice.
func (m *Manager) fire(id string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok || m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.entries, id)
	queue := m.queue
	m.mu.Unlock()

	r := e.reminder
	eventID, err := m.beginFire(r)
	if err != nil {
		if errors.Is(err, errAlreadyGone) {
			// A cancel won the race before the transaction reached the row.
			return
		}
		// Without a committed delete the Reminder may still be pending; saying
		// it now could repeat it after a restart. Leave it for recovery.
		m.logger.Error("reminder fire history", "reminder_id", r.ID, "error", err.Error())
		return
	}
	// A due Reminder also reaches the phone. Send runs after the durable fire
	// transaction and on every branch below, so a missing speaker or queue
	// cannot suppress it. The Notification service applies notify.reminders
	// and Quiet hours itself.
	defer m.notifyDue(r.Message)
	if queue == nil {
		m.finishEvent(eventID, alertstore.StateMissed, "unavailable")
		m.logger.Warn("reminder dropped: the Announcement queue is not configured", "reminder_id", r.ID)
		return
	}
	// A fired Reminder has no pending row, so its only cancel handle is this
	// playback context, kept until the queue item finishes.
	activeCtx, activeCancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		activeCancel()
		m.finishEvent(eventID, alertstore.StateInterrupted, "shutdown")
		return
	}
	m.active[r.ID] = activeCancel
	m.activeMessages[r.ID] = r.Message
	m.mu.Unlock()
	release := func() {
		activeCancel()
		m.mu.Lock()
		delete(m.active, r.ID)
		delete(m.cancelled, r.ID)
		delete(m.activeMessages, r.ID)
		m.mu.Unlock()
	}
	play := func(queueCtx context.Context) error {
		// The active entry lives until the queue worker finishes this item, so
		// a cancel can reach a message that is still waiting its turn.
		defer release()
		// Mirror a manager cancel into the queue's own barge-in context.
		ctx, cancel := context.WithCancel(queueCtx)
		defer cancel()
		stop := context.AfterFunc(activeCtx, cancel)
		defer stop()
		return m.speakAlert(ctx, eventID, r)
	}
	if !queue.EnqueueAlert(play) {
		release()
		m.finishEvent(eventID, alertstore.StateMissed, "unavailable")
		m.logger.Warn("reminder dropped: the Announcement queue is closed", "reminder_id", r.ID)
	}
}

// speakAlert runs on the Announcement worker as one item: the Cue first, then
// the message, so a turn cannot come between the sound and its speech.
func (m *Manager) speakAlert(ctx context.Context, eventID int64, r Reminder) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		// Shutdown won the race with the queue. The history row stays honest
		// and the next start records the Reminder as missed rather than speech.
		m.finishEvent(eventID, alertstore.StateInterrupted, "shutdown")
		return nil
	}
	if m.ringCancelled(r.ID) {
		// A dashboard or voice cancel was asked for before this item reached
		// the worker; record the user's intent, not a barge-in.
		m.finishEvent(eventID, alertstore.StateStopped, "cancel")
		return nil
	}
	m.markRinging(eventID)
	if err := m.playCue(ctx); err != nil {
		if ctx.Err() != nil {
			m.finishInterrupted(eventID, r.ID)
			return err
		}
		// A missing or broken Cue must not drop the spoken message.
		m.logger.Error("reminder cue", "reminder_id", r.ID, "error", err.Error())
	}
	if m.speaker == nil {
		m.finishEvent(eventID, alertstore.StateMissed, "unavailable")
		m.logger.Warn("reminder dropped: speech is not configured", "reminder_id", r.ID)
		return errors.New("reminder: speech is not configured")
	}
	if err := m.speak(ctx, r.Message); err != nil {
		if ctx.Err() != nil {
			m.finishInterrupted(eventID, r.ID)
			return err
		}
		m.finishEvent(eventID, alertstore.StateMissed, "unavailable")
		m.logger.Error("reminder speech", "reminder_id", r.ID, "error", err.Error())
		return err
	}
	m.finishEvent(eventID, alertstore.StateStopped, "spoken")
	m.logger.Info("reminder spoken", "reminder_id", r.ID)
	return nil
}

// ringCancelled reports whether a cancel asked for this fired Reminder.
func (m *Manager) ringCancelled(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled[id]
}

// notifyDue pushes one due Reminder to the phone. A failure is already logged
// by the Notification service and never changes the spoken outcome.
func (m *Manager) notifyDue(message string) {
	if m.notify == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.notify.NotifyReminder(ctx, message)
}

// finishInterrupted records a cancelled playback as a user cancel when the
// manager stopped it, or a barge-in otherwise.
func (m *Manager) finishInterrupted(eventID int64, id string) {
	if m.ringCancelled(id) {
		m.finishEvent(eventID, alertstore.StateStopped, "cancel")
		return
	}
	m.finishEvent(eventID, alertstore.StateInterrupted, "barge_in")
}

// playCue resolves the live Cue, scales it for Quiet hours and plays it once.
func (m *Manager) playCue(ctx context.Context) error {
	if m.cues == nil {
		return nil
	}
	clip, ok := m.cues.Clip(sound.RoleReminder)
	if !ok {
		return nil
	}
	if m.quiet != nil {
		clip = m.quiet.Clip(clip)
	}
	return m.cues.PlayClip(ctx, clip)
}

// speak says the message through the Quiet-hours seam, so the selected voice,
// rate and quiet.volume all apply. There is no model call on this path.
func (m *Manager) speak(ctx context.Context, text string) error {
	if m.quiet != nil {
		return m.quiet.Speak(ctx, m.speaker, text)
	}
	return m.speaker.SayText(ctx, text)
}

// List returns the active Reminders, soonest first.
func (m *Manager) List() []Reminder {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	loc := m.location()
	out := make([]Reminder, 0, len(m.entries))
	for _, e := range m.entries {
		r := e.reminder
		r.RemainingSeconds = remainingSeconds(r.FiresAt, now)
		r.When = whenText(r, now, loc)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FiresAt.Before(out[j].FiresAt) })
	return out
}

// Cancel stops one Reminder by id. It returns how many were removed.
func (m *Manager) Cancel(id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for key := range m.entries {
		if id == "" || key == id {
			ids = append(ids, key)
		}
	}
	cancelled, err := m.cancelLocked(ids, "reminder_id")
	if err != nil {
		return cancelled, err
	}
	if id != "" {
		if cancel := m.active[id]; cancel != nil && !m.cancelled[id] {
			// A fired Reminder has no pending row; mark the active playback so
			// its history row records stopped/cancel, then stop it. The flag
			// dedupes a second cancel before the queue item releases.
			m.cancelled[id] = true
			cancel()
			cancelled++
			m.logger.Info("reminder playback cancelled", "reminder_id", id)
		}
	}
	return cancelled, nil
}

// CancelQuery stops every Reminder whose message contains query. An empty
// query cancels them all, which is what "cancel the reminder" means.
func (m *Manager) CancelQuery(query string) (int, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for key, e := range m.entries {
		if query == "" || strings.Contains(strings.ToLower(e.reminder.Message), query) {
			ids = append(ids, key)
		}
	}
	cancelled, err := m.cancelLocked(ids, "query")
	if err != nil {
		return cancelled, err
	}
	// A fired Reminder has no pending row. Match its retained message and stop
	// the playback or queued item too; an empty query means "cancel every
	// reminder", which must include the one already speaking.
	for id, message := range m.activeMessages {
		if query != "" && !strings.Contains(strings.ToLower(message), query) {
			continue
		}
		if cancel := m.active[id]; cancel != nil && !m.cancelled[id] {
			m.cancelled[id] = true
			cancel()
			cancelled++
			m.logger.Info("reminder playback cancelled", "reminder_id", id)
		}
	}
	return cancelled, nil
}

// cancelLocked removes the persisted rows then the live entries. The caller
// holds the write lock, so a callback cannot fire between the two steps.
func (m *Manager) cancelLocked(ids []string, attr string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		if err := m.deleteRows(ctx, ids); err != nil {
			return 0, err
		}
	}
	for _, key := range ids {
		if e, ok := m.entries[key]; ok {
			e.stop.Stop()
			delete(m.entries, key)
		}
	}
	// Only the count and the match kind: a cancel query can itself be a piece
	// of the reminder message, which must never reach the log.
	m.logger.Info("reminder cancelled", "match", attr, "count", len(ids))
	return len(ids), nil
}

// History returns the newest alert-history rows across every kind. It is the
// read seam for the Scheduled alerts page (#58) and later Notification work;
// the Reminder rows carry kind "reminder".
func (m *Manager) History(limit int) ([]alertstore.Event, error) {
	m.mu.Lock()
	history := m.history
	m.mu.Unlock()
	if history == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	return history.List(ctx, limit)
}

// Close stops every callback and cancels an active alert without deleting the
// pending rows. It waits for an in-flight fire transaction, so the caller may
// close the database afterwards.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for id, e := range m.entries {
		e.stop.Stop()
		delete(m.entries, id)
	}
	for id, cancel := range m.active {
		cancel()
		delete(m.active, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// remainingSeconds clamps a due time to a non-negative whole-second count.
func remainingSeconds(firesAt, now time.Time) int {
	remaining := firesAt.Sub(now)
	if remaining < 0 {
		return 0
	}
	return int(remaining.Round(time.Second) / time.Second)
}

// whenText is the spoken due time at now: relative for a duration Reminder,
// local clock for a clock-time Reminder.
func whenText(r Reminder, now time.Time, loc *time.Location) string {
	if r.Kind == KindDuration {
		if !r.FiresAt.After(now) {
			return "now"
		}
		return "in " + parse.HumanDuration(r.FiresAt.Sub(now))
	}
	if loc == nil {
		loc = time.UTC
	}
	local := r.FiresAt.In(loc)
	clock := quiet.HumanClock(fmt.Sprintf("%02d:%02d", local.Hour(), local.Minute()))
	today := now.In(loc)
	switch {
	case sameDate(local, today):
		return "at " + clock
	case sameDate(local, today.AddDate(0, 0, 1)):
		return "tomorrow at " + clock
	default:
		return "on " + local.Format("Mon") + " at " + clock
	}
}

// sameDate compares the calendar date in one location.
func sameDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
