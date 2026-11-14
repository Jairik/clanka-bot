package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// ModuleProvider returns the one wired Tool module with name, or nil. tools.Set
// satisfies it; the Scheduled alerts page reaches the Timer, Alarm and
// Reminder managers through that one list instead of reviving a per-domain
// Options field for every manager.
type ModuleProvider interface {
	Module(name string) tool.Module
}

const (
	// alertSoundDefault is the select value that clears a per-Alarm sound
	// override back to sounds.alarm_ring. An empty value means "keep current",
	// so clearing and not touching the field are different requests.
	alertSoundDefault = "__default"

	// alertHistoryLimit bounds the shared alert-history read. The page only
	// needs a recent window; an unbounded query would grow with the database.
	alertHistoryLimit = 50
	// missedDisplayLimit bounds the missed section, so an old noisy month
	// cannot turn one page into an unbounded list.
	missedDisplayLimit = 20

	// Alert page flash values. They are internal names, never user text.
	flashAlertCancelled = "alert-cancelled"
	flashAlertGone      = "alert-gone"
	flashAlertSaved     = "alert-saved"
	flashAlertFallback  = "alert-sound-fallback"
	flashAlertReset     = "alert-reset"
	flashAlertUnchanged = "alert-unchanged"
)

// alertsView is the Scheduled alerts page data model. Every value is
// precomputed, so the template contains no lookups or comparisons.
type alertsView struct {
	Owner       string
	Page        string
	Flash       string
	Error       string
	HasManagers bool
	ZoneName    string
	Alerts      []scheduledAlertView
	Alarms      []alarmEditorView
	Missed      []missedAlertView
	MissedLimit int
}

// scheduledAlertView is one active Timer, Alarm or Reminder row.
type scheduledAlertView struct {
	KindKey      string
	Kind         string
	Label        string
	Next         string
	NextISO      string
	Detail       string
	CancelAction string
	CancelLabel  string

	// firesAt is only used to sort the one combined list.
	firesAt time.Time
}

// alarmEditorView is one Alarm and its per-Alarm overrides. Current is the
// stored override; Effective is what the Ring will actually use after the
// kind/default layer is applied.
type alarmEditorView struct {
	ID              string
	Title           string
	Schedule        string
	Next            string
	NextISO         string
	SoundCurrent    string
	SoundEffective  string
	SoundDefault    string
	SoundOverride   bool
	VolumeCurrent   string
	VolumeEffective string
	VolumeDefault   string
	VolumeOverride  bool
	RampCurrent     string
	RampEffective   string
	RampDefault     string
	RampOverride    bool
	HasOverride     bool
	SoundKey        string
	VolumeKey       string
	RampKey         string
	SoundCap        string
	HasLibrary      bool
	Sounds          []soundOption
	Action          string
	ResetAction     string
	Edit            alertEditState
}

// alertEditState preserves a rejected form submission for one Alarm. It is
// also present with empty values on a normal render, which selects "keep
// current" in the sound picker.
type alertEditState struct {
	AlarmID     string
	Sound       string
	Volume      string
	Ramp        string
	VolumeReset bool
	RampReset   bool
}

// alertsDeps is the typed set of managers one Scheduled alerts request uses.
// It is resolved once from the one module list, so the handlers never rebuild
// a per-domain Options field forest.
type alertsDeps struct {
	timers    *timer.Manager
	alarms    *alarm.Manager
	reminders *reminder.Manager
}

// missedAlertView is one missed row from the shared alert history.
type missedAlertView struct {
	Kind         string
	Label        string
	Scheduled    string
	ScheduledISO string
	Fired        string
	FiredISO     string
	Ended        string
	EndedISO     string
	EndedBy      string
}

// alertsPage renders the active alerts, the per-Alarm override editors and the
// shared missed history.
func (h *Handler) alertsPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildAlertsView(alertsFlashMessage(r.URL.Query().Get("flash")), "", nil))
}

// alertCancel cancels exactly one alert through its own manager. The id comes
// from the path, so an empty selector can never cancel every alert, and the
// manager stops its own active Ring by that id only.
func (h *Handler) alertCancel(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	deps := h.alertDeps()
	var (
		cancelled int
		err       error
	)
	switch kind {
	case "timer":
		if deps.timers == nil {
			http.NotFound(w, r)
			return
		}
		cancelled, err = deps.timers.CancelTimers(id)
	case "alarm":
		if deps.alarms == nil {
			http.NotFound(w, r)
			return
		}
		cancelled, err = deps.alarms.Cancel(r.Context(), alarm.CancelIn{ID: id})
	case "reminder":
		if deps.reminders == nil {
			http.NotFound(w, r)
			return
		}
		cancelled, err = deps.reminders.Cancel(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.Error("alert cancel", "kind", kind, "alert_id", id, "error", err.Error())
		h.alertsFail(w, r, http.StatusInternalServerError, "Could not cancel that alert.", nil)
		return
	}
	if cancelled == 0 {
		h.alertsSuccess(w, r, flashAlertGone)
		return
	}
	h.logger.Info("alert cancelled from dashboard", "kind", kind, "alert_id", id)
	h.alertsSuccess(w, r, flashAlertCancelled)
}

// alertAlarmUpdate writes the per-Alarm sound, volume and ramp through
// Manager.Update, the same typed path alarm.update uses. All fields are
// parsed and validated before the one manager call, so a rejected form cannot
// write some fields and leave others behind.
func (h *Handler) alertAlarmUpdate(w http.ResponseWriter, r *http.Request) {
	manager := h.alertDeps().alarms
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.alertsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	in, edit, changed, err := parseAlarmOverrideForm(r, id)
	if err != nil {
		h.alertsFail(w, r, http.StatusBadRequest, err.Error(), edit)
		return
	}
	if !changed {
		h.alertsSuccess(w, r, flashAlertUnchanged)
		return
	}
	out, err := manager.Update(r.Context(), in)
	if err != nil {
		h.logger.Error("alert override save", "alarm_id", id, "error", err.Error())
		h.alertsFail(w, r, http.StatusBadRequest, alertPhoneSafeError(err), edit)
		return
	}
	if out.Ambiguous {
		h.alertsFail(w, r, http.StatusConflict, "More than one alarm matched; nothing was changed.", edit)
		return
	}
	if out.Matched == 0 {
		h.alertsSuccess(w, r, flashAlertGone)
		return
	}
	flash := flashAlertSaved
	if out.SoundFallback {
		flash = flashAlertFallback
	}
	h.logger.Info("alarm overrides saved from dashboard", "alarm_id", id, "changed", out.Changed)
	h.alertsSuccess(w, r, flash)
}

// alertAlarmReset clears every per-Alarm override in one manager call. Empty
// means NULL/kind-default here, never an explicit 0.
func (h *Handler) alertAlarmReset(w http.ResponseWriter, r *http.Request) {
	manager := h.alertDeps().alarms
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	out, err := manager.Update(r.Context(), alarm.UpdateIn{
		ID:          id,
		SoundReset:  true,
		VolumeReset: true,
		RampReset:   true,
	})
	if err != nil {
		h.logger.Error("alert override reset", "alarm_id", id, "error", err.Error())
		h.alertsFail(w, r, http.StatusBadRequest, alertPhoneSafeError(err), nil)
		return
	}
	if out.Matched == 0 {
		h.alertsSuccess(w, r, flashAlertGone)
		return
	}
	h.logger.Info("alarm overrides reset from dashboard", "alarm_id", id)
	h.alertsSuccess(w, r, flashAlertReset)
}

// parseAlarmOverrideForm turns the ordinary form into the typed UpdateIn.
// Empty fields mean "keep", the default sound value means SoundReset, and a
// numeric 0 is kept as an explicit 0. The returned edit state is the rejected
// input the page renders back into the same Alarm.
func parseAlarmOverrideForm(r *http.Request, id string) (alarm.UpdateIn, *alertEditState, bool, error) {
	edit := &alertEditState{
		AlarmID:     id,
		Sound:       strings.TrimSpace(r.FormValue("sound")),
		Volume:      strings.TrimSpace(r.FormValue("volume")),
		Ramp:        strings.TrimSpace(r.FormValue("ramp")),
		VolumeReset: r.FormValue("volume_reset") != "",
		RampReset:   r.FormValue("ramp_reset") != "",
	}
	in := alarm.UpdateIn{ID: id}
	changed := false

	switch edit.Sound {
	case "":
		// Keep the current sound override.
	case alertSoundDefault:
		in.SoundReset = true
		changed = true
	default:
		in.Sound = edit.Sound
		changed = true
	}

	if edit.VolumeReset {
		if edit.Volume != "" {
			return in, edit, false, errors.New("Choose a volume or the default, not both.")
		}
		in.VolumeReset = true
		changed = true
	} else if edit.Volume != "" {
		// The numeric range is the Manager's existing validation; the page only
		// turns the ordinary form field into the typed pointer. An explicit 0
		// stays a pointer, so mute is distinct from the NULL default.
		volume, err := strconv.Atoi(edit.Volume)
		if err != nil {
			return in, edit, false, errors.New("Volume must be a whole number from 0 to 100.")
		}
		in.Volume = &volume
		changed = true
	}

	if edit.RampReset {
		if edit.Ramp != "" {
			return in, edit, false, errors.New("Choose a ramp or the default, not both.")
		}
		in.RampReset = true
		changed = true
	} else if edit.Ramp != "" {
		// The Manager owns the 0..ring.MaxRamp range; this only rejects a
		// non-number while keeping the submitted text for the rendered form.
		seconds, err := strconv.Atoi(edit.Ramp)
		if err != nil {
			return in, edit, false, fmt.Errorf("Ramp must be a whole number of seconds from 0 to %d.", int(ring.MaxRamp/time.Second))
		}
		in.RampSeconds = &seconds
		changed = true
	}
	return in, edit, changed, nil
}

// alertPhoneSafeError reports only the user-facing ToolError text. An internal
// error could name a database path or another local detail, so it stays in the
// log and the page gets one stable sentence.
func alertPhoneSafeError(err error) string {
	var toolErr *tool.ToolError
	if errors.As(err, &toolErr) && strings.TrimSpace(toolErr.Say) != "" {
		return strings.TrimSpace(toolErr.Say)
	}
	return "Could not save the alarm overrides."
}

// alertsSuccess answers with the refreshed view, so the app can replace the
// page state from one response.
func (h *Handler) alertsSuccess(w http.ResponseWriter, r *http.Request, flash string) {
	h.writeJSON(w, http.StatusOK, h.buildAlertsView(alertsFlashMessage(flash), "", nil))
}

// alertsFail answers with an error and the rejected input. The status is the
// real one, so the app can tell a refused write from a saved one.
func (h *Handler) alertsFail(w http.ResponseWriter, r *http.Request, status int, message string, edit *alertEditState) {
	view := h.buildAlertsView("", message, edit)
	h.writeJSON(w, status, view)
}

// alertsFlashMessage maps the closed flash set to the user-visible result.
func alertsFlashMessage(flash string) string {
	switch flash {
	case flashAlertCancelled:
		return "Cancelled. That Scheduled alert will not go off."
	case flashAlertGone:
		return "That alert was already gone; nothing was cancelled."
	case flashAlertSaved:
		return "Saved. The Alarm Rings with these overrides on its next occurrence."
	case flashAlertFallback:
		return "Saved, but that sound was not found, so the kind default applies."
	case flashAlertReset:
		return "Reset. The Alarm uses sounds.alarm_ring and ring.alarm.* again."
	case flashAlertUnchanged:
		return "No changes."
	default:
		return ""
	}
}

// buildAlertsView resolves the three managers, the per-Alarm effective values
// and the bounded missed history from one snapshot.
func (h *Handler) buildAlertsView(flash, errorText string, edit *alertEditState) alertsView {
	view := alertsView{
		Owner:       h.owner,
		Page:        "alerts",
		Flash:       flash,
		Error:       errorText,
		ZoneName:    h.alertZoneName(),
		MissedLimit: missedDisplayLimit,
	}
	deps := h.alertDeps()
	timers, alarms, reminders := deps.timers, deps.alarms, deps.reminders
	view.HasManagers = timers != nil || alarms != nil || reminders != nil
	if !view.HasManagers {
		return view
	}

	now := h.now()
	loc := h.alertLocation()
	// One bounded history read feeds both the open fired/ringing rows and the
	// missed section, so three managers returning the same shared table cannot
	// duplicate a row.
	events := h.alertHistory(deps)
	pending := make(map[string]bool)
	live := make(map[string]bool)
	if timers != nil {
		for _, item := range timers.List() {
			pending[alertKey("timer", item.ID)] = true
			label := item.Label
			if label == "" {
				label = "—"
			}
			detail := fmt.Sprintf("%s of %s left", remainingLabel(item.RemainingSeconds), displayDuration(time.Duration(item.DurationSeconds)*time.Second))
			view.Alerts = append(view.Alerts, scheduledAlertView{
				KindKey:      "timer",
				Kind:         "Timer",
				Label:        label,
				Next:         formatAlertTime(item.FiresAt, now, loc),
				NextISO:      item.FiresAt.UTC().Format(time.RFC3339),
				Detail:       detail,
				CancelAction: alertCancelAction("timer", item.ID),
				CancelLabel:  alertCancelLabel("timer", item.Label, item.FiresAt, now, loc),
				firesAt:      item.FiresAt,
			})
		}
	}
	if alarms != nil {
		for _, item := range alarms.List() {
			pending[alertKey("alarm", item.ID)] = true
			label := item.Label
			if label == "" {
				label = "—"
			}
			view.Alerts = append(view.Alerts, scheduledAlertView{
				KindKey:      "alarm",
				Kind:         "Alarm",
				Label:        label,
				Next:         formatAlertTime(item.NextFiresAt, now, loc),
				NextISO:      item.NextFiresAt.UTC().Format(time.RFC3339),
				Detail:       describeAlarmSchedule(item),
				CancelAction: alertCancelAction("alarm", item.ID),
				CancelLabel:  alertCancelLabel("alarm", item.Label, item.NextFiresAt, now, loc),
				firesAt:      item.NextFiresAt,
			})
		}
	}
	if reminders != nil {
		for _, item := range reminders.List() {
			pending[alertKey("reminder", item.ID)] = true
			detail := "Speaks once"
			if item.When != "" {
				detail += " · " + item.When
			}
			view.Alerts = append(view.Alerts, scheduledAlertView{
				KindKey:      "reminder",
				Kind:         "Reminder",
				Label:        item.Message,
				Next:         formatAlertTime(item.FiresAt, now, loc),
				NextISO:      item.FiresAt.UTC().Format(time.RFC3339),
				Detail:       detail,
				CancelAction: alertCancelAction("reminder", item.ID),
				CancelLabel:  alertCancelLabel("reminder", item.Message, item.FiresAt, now, loc),
				firesAt:      item.FiresAt,
			})
		}
	}
	// A fired alert has already deleted its pending row. Its open history row
	// is the only remaining record, so show it as an active row that can still
	// be cancelled by the owning manager; skip a repeating Alarm's open row
	// because its pending next occurrence is already listed above.
	for _, event := range events {
		if !event.State.Open() {
			continue
		}
		switch event.Kind {
		case "timer":
			if timers == nil {
				continue
			}
		case "alarm":
			if alarms == nil {
				continue
			}
		case "reminder":
			if reminders == nil {
				continue
			}
		default:
			continue
		}
		key := alertKey(event.Kind, event.SourceID)
		if pending[key] || live[key] {
			continue
		}
		live[key] = true
		label := event.Label
		if label == "" {
			label = "—"
		}
		at := event.FiresAt
		if at.IsZero() {
			at = event.FiredAt
		}
		detail := "Ringing now"
		if event.State == alertstore.StateFired {
			detail = "Fired; waiting for the speaker"
		}
		view.Alerts = append(view.Alerts, scheduledAlertView{
			KindKey:      event.Kind,
			Kind:         alertKindName(event.Kind),
			Label:        label,
			Next:         formatAlertTime(at, now, loc),
			NextISO:      at.UTC().Format(time.RFC3339),
			Detail:       detail,
			CancelAction: alertCancelAction(event.Kind, event.SourceID),
			CancelLabel:  alertCancelLabel(event.Kind, event.Label, at, now, loc),
			firesAt:      at,
		})
	}
	sort.SliceStable(view.Alerts, func(i, j int) bool {
		return view.Alerts[i].firesAt.Before(view.Alerts[j].firesAt)
	})

	// The override editor lists only pending Alarms. A one-off that already
	// fired has no next occurrence to edit, so it stays a cancel-only row.
	if alarms != nil {
		view.Alarms = h.buildAlarmEditors(alarms.List(), edit)
	}

	view.Missed = h.buildMissedAlerts(events)
	return view
}

// alertKey identifies one alert kind and its manager/source id for dedupe.
func alertKey(kind, id string) string {
	return kind + "\x00" + id
}

// buildAlarmEditors renders one editor per Alarm with the current override
// and the effective value the Ring will use.
func (h *Handler) buildAlarmEditors(alarms []alarm.Alarm, edit *alertEditState) []alarmEditorView {
	effective := ring.ReadSettings(h.settings, ring.Alarm)
	defaultSound := "none"
	if h.settings != nil {
		if name := strings.TrimSpace(h.settings.String(ring.SoundKey(ring.Alarm))); name != "" {
			defaultSound = name
		}
	}
	defaultVolume := fmt.Sprintf("%d%%", effective.Volume)
	defaultRamp := durationText(effective.Ramp)
	out := make([]alarmEditorView, 0, len(alarms))
	for _, item := range alarms {
		clock := clockText(item.Hour, item.Minute)
		title := "Alarm at " + clock
		if item.Label != "" {
			title = item.Label + " · " + clock
		}
		state := alertEditState{}
		if edit != nil && edit.AlarmID == item.ID {
			state = *edit
		}
		// Effective is what this Alarm will actually use: the stored override
		// when present, otherwise the kind/default layer. The default is shown
		// separately so clearing is unambiguous.
		soundEffective := defaultSound
		if item.Sound != "" {
			soundEffective = item.Sound
		}
		volumeEffective := defaultVolume
		if item.Volume != nil {
			volumeEffective = fmt.Sprintf("%d%%", *item.Volume)
		}
		rampEffective := defaultRamp
		if item.RampSeconds != nil {
			rampEffective = durationText(time.Duration(*item.RampSeconds) * time.Second)
		}
		editor := alarmEditorView{
			ID:              item.ID,
			Title:           title,
			Schedule:        describeAlarmSchedule(item),
			Next:            formatAlertTime(item.NextFiresAt, h.now(), h.alertLocation()),
			NextISO:         item.NextFiresAt.UTC().Format(time.RFC3339),
			SoundCurrent:    currentSound(item.Sound),
			SoundEffective:  soundEffective,
			SoundDefault:    defaultSound,
			SoundOverride:   item.Sound != "",
			VolumeCurrent:   currentVolume(item.Volume),
			VolumeEffective: volumeEffective,
			VolumeDefault:   defaultVolume,
			VolumeOverride:  item.Volume != nil,
			RampCurrent:     currentRamp(item.RampSeconds),
			RampEffective:   rampEffective,
			RampDefault:     defaultRamp,
			RampOverride:    item.RampSeconds != nil,
			SoundKey:        ring.SoundKey(ring.Alarm),
			VolumeKey:       ring.Key(ring.Alarm, "volume"),
			RampKey:         ring.Key(ring.Alarm, "ramp"),
			SoundCap:        displayDuration(sound.Cap(sound.RoleAlarmRing)),
			HasLibrary:      h.sounds != nil,
			Action:          "/alerts/alarms/" + url.PathEscape(item.ID),
			ResetAction:     "/alerts/alarms/" + url.PathEscape(item.ID) + "/reset",
			Edit:            state,
		}
		editor.HasOverride = editor.SoundOverride || editor.VolumeOverride || editor.RampOverride
		editor.Sounds = h.alarmSoundOptions(item.Sound, state.Sound)
		out = append(out, editor)
	}
	return out
}

// alarmSoundOptions builds the picker: keep the current override, clear back
// to sounds.alarm_ring, or choose one library sound. A current name missing
// from the library is still shown so a stale row is honest and removable.
func (h *Handler) alarmSoundOptions(current, edited string) []soundOption {
	keep := "Keep current"
	if current != "" {
		keep = "Keep current (" + current + ")"
	}
	options := []soundOption{{Value: "", Label: keep}}
	if h.sounds == nil {
		return options
	}
	options = append(options, soundOption{Value: alertSoundDefault, Label: "Default (sounds.alarm_ring)"})
	found := false
	for _, entry := range h.sounds.List() {
		if entry.Name == "" {
			continue
		}
		if entry.Name == current {
			found = true
		}
		options = append(options, soundOption{
			Value: entry.Name,
			Label: entry.Name + " (" + displayDuration(entry.Duration) + ")",
		})
	}
	if current != "" && !found {
		options = append(options, soundOption{Value: current, Label: current + " (missing from library)"})
	}
	for i := range options {
		options[i].Selected = options[i].Value == edited
	}
	return options
}

// alertHistory reads the shared alert history once. Every manager wraps the
// same alert_events table, so asking all three would list every row three
// times; one source is enough and the kind column keeps it readable.
func (h *Handler) alertHistory(deps alertsDeps) []alertstore.Event {
	var events []alertstore.Event
	var err error
	switch {
	case deps.alarms != nil:
		events, err = deps.alarms.History(alertHistoryLimit)
	case deps.timers != nil:
		events, err = deps.timers.History(alertHistoryLimit)
	case deps.reminders != nil:
		events, err = deps.reminders.History(alertHistoryLimit)
	}
	if err != nil {
		h.logger.Warn("alert history read failed", "error", err.Error())
		return nil
	}
	return events
}

// buildMissedAlerts filters the one bounded history read to missed rows.
func (h *Handler) buildMissedAlerts(events []alertstore.Event) []missedAlertView {
	now := h.now()
	loc := h.alertLocation()
	out := make([]missedAlertView, 0, missedDisplayLimit)
	for _, event := range events {
		if event.State != alertstore.StateMissed {
			continue
		}
		if len(out) >= missedDisplayLimit {
			break
		}
		label := event.Label
		if label == "" {
			label = "—"
		}
		ended := "—"
		endedISO := ""
		if !event.EndedAt.IsZero() {
			ended = formatAlertTime(event.EndedAt, now, loc)
			endedISO = event.EndedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, missedAlertView{
			Kind:         alertKindName(event.Kind),
			Label:        label,
			Scheduled:    formatAlertTime(event.FiresAt, now, loc),
			ScheduledISO: event.FiresAt.UTC().Format(time.RFC3339),
			Fired:        formatAlertTime(event.FiredAt, now, loc),
			FiredISO:     event.FiredAt.UTC().Format(time.RFC3339),
			Ended:        ended,
			EndedISO:     endedISO,
			EndedBy:      event.EndedBy,
		})
	}
	return out
}

// alertDeps resolves the three managers once from the one module list. A
// Dashboard built without the module list simply renders no page.
func (h *Handler) alertDeps() alertsDeps {
	return alertsDeps{
		timers:    h.timerManager(),
		alarms:    h.alarmManager(),
		reminders: h.reminderManager(),
	}
}

// timerManager, alarmManager and reminderManager are the one module-list
// lookup behind alertDeps.
func (h *Handler) timerManager() *timer.Manager {
	if h.toolModules == nil {
		return nil
	}
	module, _ := h.toolModules.Module("timer").(*timer.Module)
	if module == nil {
		return nil
	}
	return module.Manager()
}

func (h *Handler) alarmManager() *alarm.Manager {
	if h.toolModules == nil {
		return nil
	}
	module, _ := h.toolModules.Module("alarm").(*alarm.Module)
	if module == nil {
		return nil
	}
	return module.Manager()
}

func (h *Handler) reminderManager() *reminder.Manager {
	if h.toolModules == nil {
		return nil
	}
	module, _ := h.toolModules.Module("reminder").(*reminder.Module)
	if module == nil {
		return nil
	}
	return module.Manager()
}

// alertLocation and alertZoneName read the live timezone on every render. A
// settings change therefore applies to the page without a restart.
func (h *Handler) alertLocation() *time.Location {
	if h.zone != nil {
		return h.zone.Location()
	}
	return time.UTC
}

func (h *Handler) alertZoneName() string {
	if h.zone != nil {
		return h.zone.Name()
	}
	return "UTC"
}

// formatAlertTime renders a time in the live zone. The RFC3339 value is kept
// beside it in the template's datetime attribute, so the exact instant is
// machine-readable without screen-reader ambiguity.
func formatAlertTime(at time.Time, now time.Time, loc *time.Location) string {
	if at.IsZero() {
		return "—"
	}
	local := at.In(loc)
	localNow := now.In(loc)
	clock := local.Format("15:04")
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	switch {
	case day.Equal(today):
		return clock
	case day.Equal(today.AddDate(0, 0, 1)):
		return "tomorrow " + clock
	case local.Year() == localNow.Year():
		return local.Format("Mon 2 Jan") + " " + clock
	default:
		return local.Format("2 Jan 2006") + " " + clock
	}
}

// clockText formats an Alarm hour/minute without the date.
func clockText(hour, minute int) string {
	return time.Date(2000, 1, 1, hour, minute, 0, 0, time.UTC).Format("3:04 pm")
}

// describeAlarmSchedule names the repeat rule the Alarm row already carries.
func describeAlarmSchedule(item alarm.Alarm) string {
	switch item.Schedule {
	case alarm.Once:
		return "Once"
	case alarm.Weekdays:
		return "Weekdays"
	case alarm.Weekends:
		return "Weekends"
	case alarm.Chosen:
		if len(item.Weekdays) == 0 {
			return "Chosen days"
		}
		return strings.Join(item.Weekdays, ", ")
	case alarm.Daily:
		return "Every day"
	default:
		return "Every day"
	}
}

func currentSound(name string) string {
	if name == "" {
		return "Default"
	}
	return name
}

func currentVolume(volume *int) string {
	if volume == nil {
		return "Default"
	}
	return fmt.Sprintf("%d%%", *volume)
}

func currentRamp(seconds *int) string {
	if seconds == nil {
		return "Default"
	}
	return durationText(time.Duration(*seconds) * time.Second)
}

// durationText differs from displayDuration only at zero: an explicit 0 s
// ramp is a real value ("0s"), not an unknown duration.
func durationText(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	return displayDuration(d)
}

func remainingLabel(seconds int) string {
	if seconds <= 0 {
		return "due now"
	}
	return displayDuration(time.Duration(seconds) * time.Second)
}

func alertKindName(kind string) string {
	switch kind {
	case "timer":
		return "Timer"
	case "alarm":
		return "Alarm"
	case "reminder":
		return "Reminder"
	default:
		return kind
	}
}

func alertCancelAction(kind, id string) string {
	return "/alerts/" + kind + "/" + url.PathEscape(id) + "/cancel"
}

func alertCancelLabel(kind, label string, at time.Time, now time.Time, loc *time.Location) string {
	what := alertKindName(kind)
	if label != "" {
		return "Cancel " + what + " " + label
	}
	return "Cancel " + what + " at " + formatAlertTime(at, now, loc)
}
