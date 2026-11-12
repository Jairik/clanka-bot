package alarm_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// testFormat is the mono PCM16 layout every test Cue uses.
var testFormat = audio.Format{Rate: 8000, Width: 2, Channels: 1}

// overrideFixture is one real store, sound library and matcher. The matcher's
// embed service is a local fake, so the name-resolution rules are exercised
// without the real MiniLM endpoint.
type overrideFixture struct {
	store *store.Store
	cues  *sound.Service
	names *namematch.Matcher
	dir   string
}

// newOverrideFixture creates a temp data volume with birds, rain and a
// deliberately over-cap long sound, then indexes it through sound.New.
func newOverrideFixture(t *testing.T) *overrideFixture {
	t.Helper()
	dir := t.TempDir()
	soundsDir := filepath.Join(dir, "sounds")
	if err := os.MkdirAll(soundsDir, 0o700); err != nil {
		t.Fatalf("create sounds dir: %v", err)
	}
	writeSoundFile(t, soundsDir, "birds", 8, 10000)
	writeSoundFile(t, soundsDir, "rain", 12, 10000)
	writeSoundFile(t, soundsDir, "long", testFormat.Rate*61, 10000)

	st := openStore(t, filepath.Join(dir, "assistant.db"))
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cues, err := sound.New(context.Background(), st, dir, testFormat, logger)
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	return &overrideFixture{store: st, cues: cues, names: newFakeMatcher(t), dir: dir}
}

// writeSoundFile writes one constant-amplitude mono PCM16 WAV.
func writeSoundFile(t *testing.T, dir, name string, samples int, amplitude int16) {
	t.Helper()
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(amplitude))
	}
	if err := os.WriteFile(filepath.Join(dir, name+".wav"), audio.EncodeWAV(pcm, testFormat), 0o600); err != nil {
		t.Fatalf("write %s.wav: %v", name, err)
	}
}

// newFakeMatcher maps the sound vocabulary onto orthogonal axes. "bird" and
// "birds" share an axis so the similarity step has a deterministic near miss;
// an unrelated name lands on its own axis and never matches.
func newFakeMatcher(t *testing.T) *namematch.Matcher {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Texts []string `json:"texts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vectors := make([][]float32, len(request.Texts))
		for i, text := range request.Texts {
			lower := strings.ToLower(text)
			vector := make([]float32, 5)
			switch {
			case strings.Contains(lower, "bird"):
				vector[0] = 1
			case strings.Contains(lower, "rain"):
				vector[1] = 1
			case strings.Contains(lower, "chime"):
				vector[2] = 1
			case strings.Contains(lower, "long"):
				vector[3] = 1
			default:
				vector[4] = 1
			}
			vectors[i] = vector
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vectors})
	}))
	t.Cleanup(server.Close)
	return namematch.New(embed.NewClient(server.URL, "fake-minilm"), namematch.Config{
		Threshold: func() float64 { return namematch.DefaultThreshold },
	})
}

// newOverrideManager opens the real database and wires the sound seams.
func newOverrideManager(t *testing.T, clock *fixedClock, fixture *overrideFixture, snooze func() time.Duration) *alarm.Manager {
	t.Helper()
	manager := alarm.NewManager(alarm.Options{
		Now:    clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cues:   fixture.cues,
		Names:  fixture.names,
		Snooze: snooze,
	})
	t.Cleanup(manager.Close)
	if err := manager.AttachStore(fixture.store, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	return manager
}

// intPtr is the explicit-value helper; nil is the default.
func intPtr(value int) *int { return &value }

// TestSetExtractsSoundVolumeAndRamp is the alarm.set Fast-path journey with
// the real name matcher and a real sound library.
func TestSetExtractsSoundVolumeAndRamp(t *testing.T) {
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	set := func(utterance string) alarm.SetOut {
		t.Helper()
		args, ok := reg.Extract(alarm.SetName, utterance)
		if !ok {
			t.Fatalf("Extract(%q) failed", utterance)
		}
		result, err := reg.Execute(context.Background(), alarm.SetName, args)
		if err != nil {
			t.Fatalf("Execute(%q): %v", utterance, err)
		}
		return result.(alarm.SetOut)
	}

	// The exact library name resolves without an embed call.
	birds := set("wake me at 7 with the birds sound")
	if birds.Hour != 7 || birds.Sound != "birds" || birds.SoundFallback {
		t.Fatalf("birds alarm = %+v, want 7:00 with the birds sound", birds)
	}
	if text, _ := reg.Speak(alarm.SetName, birds); !strings.Contains(text, "with the birds sound") {
		t.Fatalf("set reply = %q, want the chosen sound", text)
	}

	// A near miss ("bird") resolves to "birds" through the shared matcher.
	bird := set("wake me at 7 with the bird sound")
	if bird.Sound != "birds" {
		t.Fatalf("near-miss sound = %q, want birds", bird.Sound)
	}

	// A volume and ramp in the same utterance are stored as explicit values.
	combined := set("set an alarm for 7 with the birds sound at volume 40 with a 10 second ramp")
	if combined.Volume == nil || *combined.Volume != 40 || combined.RampSeconds == nil || *combined.RampSeconds != 10 {
		t.Fatalf("combined alarm = %+v, want volume 40 and ramp 10", combined)
	}
	if text, _ := reg.Speak(alarm.SetName, combined); !strings.Contains(text, "at 40% volume") || !strings.Contains(text, "a 10 second ramp") {
		t.Fatalf("combined reply = %q, want the volume and ramp", text)
	}

	// An unknown name stores the kind default and says so. It must not name a
	// different library sound.
	unknown := set("wake me at 7 with the cowbell sound")
	if unknown.Sound != "" || !unknown.SoundFallback {
		t.Fatalf("unknown alarm = %+v, want the default sound and a fallback", unknown)
	}
	if text, _ := reg.Speak(alarm.SetName, unknown); !strings.Contains(text, "couldn't find that sound") {
		t.Fatalf("unknown reply = %q, want the fallback sentence", text)
	}
	if text, _ := reg.Speak(alarm.SetName, unknown); strings.Contains(text, "birds") || strings.Contains(text, "rain") || strings.Contains(text, "chime") {
		t.Fatalf("unknown reply guessed a sound: %q", text)
	}

	// An over-cap library sound is refused before the Alarm is written.
	args, ok := reg.Extract(alarm.SetName, "wake me at 9 with the long sound")
	if !ok {
		t.Fatal("long-sound Extract failed")
	}
	if _, err := reg.Execute(context.Background(), alarm.SetName, args); err == nil {
		t.Fatal("an over-cap sound was accepted")
	}
	for _, saved := range manager.List() {
		if saved.Hour == 9 {
			t.Fatalf("over-cap Alarm was written: %+v", saved)
		}
	}

	// A plain wake phrase has no overrides.
	plain := set("wake me at 7")
	if plain.Sound != "" || plain.Volume != nil || plain.RampSeconds != nil {
		t.Fatalf("plain alarm = %+v, want no overrides", plain)
	}

	// Explicit zero values stay pointers, distinct from the kind default.
	zeroVolume := set("wake me at 7 with volume 0")
	if zeroVolume.Volume == nil || *zeroVolume.Volume != 0 {
		t.Fatalf("zero volume = %+v, want explicit 0", zeroVolume.Volume)
	}
	zeroRamp := set("wake me at 8 with no ramp")
	if zeroRamp.RampSeconds == nil || *zeroRamp.RampSeconds != 0 {
		t.Fatalf("zero ramp = %+v, want explicit 0", zeroRamp.RampSeconds)
	}

	// A set request cannot be read as an update, and a wake request cannot be
	// read as an update.
	if _, ok := reg.Extract(alarm.SetName, "change my 7 am alarm to the birds sound"); ok {
		t.Error("an update phrase extracted as alarm.set")
	}
	if _, ok := reg.Extract(alarm.UpdateName, "wake me at 7 with the birds sound"); ok {
		t.Error("a wake phrase extracted as alarm.update")
	}
	if _, ok := reg.Extract(alarm.SetName, "set my alarm volume to 40"); ok {
		t.Error("setting the alarm volume extracted as alarm.set")
	}
	if _, ok := reg.Extract(alarm.CancelName, "remove the sound from my 7 am alarm"); ok {
		t.Error("removing the sound extracted as alarm.cancel")
	}
}

// TestUpdateExtractsSelectorsAndOverrides covers the Fast-path argument shape.
func TestUpdateExtractsSelectorsAndOverrides(t *testing.T) {
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	extract := func(utterance string) alarm.UpdateIn {
		t.Helper()
		args, ok := reg.Extract(alarm.UpdateName, utterance)
		if !ok {
			t.Fatalf("Extract(%q) failed", utterance)
		}
		var in alarm.UpdateIn
		if err := json.Unmarshal(args, &in); err != nil {
			t.Fatalf("decode %q: %v", utterance, err)
		}
		return in
	}

	change := extract("change my 7 am alarm to the birds sound")
	if change.Time != "07:00" || change.Sound != "birds" {
		t.Fatalf("change = %+v, want 07:00 and birds", change)
	}
	volume := extract("change my alarm volume to 40")
	if volume.Volume == nil || *volume.Volume != 40 || volume.Time != "" {
		t.Fatalf("volume = %+v, want volume 40 and no time selector", volume)
	}
	ramp := extract("change my alarm ramp to 10 seconds")
	if ramp.RampSeconds == nil || *ramp.RampSeconds != 10 || ramp.Time != "" {
		t.Fatalf("ramp = %+v, want ramp 10 and no time selector", ramp)
	}
	reset := extract("remove the sound from my 7 am alarm")
	if !reset.SoundReset || reset.Time != "07:00" {
		t.Fatalf("reset = %+v, want a sound reset on 07:00", reset)
	}
	// An unsupported time change is not an override and must fall through.
	if _, ok := reg.Extract(alarm.UpdateName, "change my 7 am alarm to 8 am"); ok {
		t.Error("a time-only change extracted as alarm.update")
	}
	// A plain change with no override is not an update.
	if _, ok := reg.Extract(alarm.UpdateName, "change my 7 am alarm"); ok {
		t.Error("an override-less change extracted as alarm.update")
	}
}

// TestUpdateSelectorsAmbiguityAndPreservation proves an ambiguous selector
// writes nothing and a single selector updates only that Alarm.
func TestUpdateSelectorsAmbiguityAndPreservation(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)

	work, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("Set work: %v", err)
	}
	gym, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "gym"})
	if err != nil {
		t.Fatalf("Set gym: %v", err)
	}
	late, err := manager.Set(ctx, alarm.SetIn{Hour: 8, Schedule: "daily", Label: "late"})
	if err != nil {
		t.Fatalf("Set late: %v", err)
	}

	// Two Alarms at 7 are ambiguous; neither may be changed.
	ambiguous, err := manager.Update(ctx, alarm.UpdateIn{Time: "07:00", Sound: "birds"})
	if err != nil {
		t.Fatalf("ambiguous Update: %v", err)
	}
	if !ambiguous.Ambiguous || ambiguous.Matched != 2 {
		t.Fatalf("ambiguous = %+v, want 2 matches", ambiguous)
	}
	for _, saved := range manager.List() {
		if saved.Sound != "" {
			t.Fatalf("ambiguous update changed %+v", saved)
		}
	}

	// Time plus label narrows to work only.
	one, err := manager.Update(ctx, alarm.UpdateIn{Time: "07:00", Label: "work", Sound: "birds"})
	if err != nil {
		t.Fatalf("narrow Update: %v", err)
	}
	if one.Matched != 1 || one.Alarm.ID != work.ID || one.Alarm.Sound != "birds" {
		t.Fatalf("narrow update = %+v, want work with birds", one)
	}
	list := manager.List()
	byID := make(map[string]alarm.Alarm, len(list))
	for _, saved := range list {
		byID[saved.ID] = saved
	}
	if byID[work.ID].Sound != "birds" {
		t.Fatalf("work sound = %q, want birds", byID[work.ID].Sound)
	}
	if byID[gym.ID].Sound != "" {
		t.Fatalf("gym sound = %q, want untouched", byID[gym.ID].Sound)
	}

	// An explicit zero volume is stored as 0, not as the default.
	if _, err := manager.Update(ctx, alarm.UpdateIn{ID: gym.ID, Volume: intPtr(0)}); err != nil {
		t.Fatalf("zero volume Update: %v", err)
	}
	var volume *int64
	if err := fixture.store.DB().QueryRow(`SELECT volume FROM alarms WHERE id = ?`, gym.ID).Scan(&volume); err != nil {
		t.Fatalf("read volume: %v", err)
	}
	if volume == nil || *volume != 0 {
		t.Fatalf("stored volume = %v, want explicit 0", volume)
	}
	// An explicit zero ramp is stored as 0 ms, not as the default.
	if _, err := manager.Update(ctx, alarm.UpdateIn{ID: late.ID, RampSeconds: intPtr(0)}); err != nil {
		t.Fatalf("zero ramp Update: %v", err)
	}
	var ramp *int64
	if err := fixture.store.DB().QueryRow(`SELECT ramp_ms FROM alarms WHERE id = ?`, late.ID).Scan(&ramp); err != nil {
		t.Fatalf("read ramp: %v", err)
	}
	if ramp == nil || *ramp != 0 {
		t.Fatalf("stored ramp = %v, want explicit 0", ramp)
	}

	// Reset returns the row to NULL, which is the kind default.
	if _, err := manager.Update(ctx, alarm.UpdateIn{ID: late.ID, SoundReset: true, VolumeReset: true, RampReset: true}); err != nil {
		t.Fatalf("reset Update: %v", err)
	}
	var (
		sound *string
		vol   *int64
		ramp2 *int64
	)
	if err := fixture.store.DB().QueryRow(
		`SELECT sound, volume, ramp_ms FROM alarms WHERE id = ?`, late.ID).Scan(&sound, &vol, &ramp2); err != nil {
		t.Fatalf("read reset row: %v", err)
	}
	if sound == nil || *sound != "" || vol != nil || ramp2 != nil {
		t.Fatalf("reset row = sound %v volume %v ramp %v, want default", sound, vol, ramp2)
	}

	// A no-op update is a clear spoken failure, not a silent write.
	if _, err := manager.Update(ctx, alarm.UpdateIn{ID: late.ID}); err == nil {
		t.Fatal("an update with no changes succeeded")
	}
	// Contradictory set/reset fields are rejected.
	if _, err := manager.Update(ctx, alarm.UpdateIn{ID: late.ID, Sound: "birds", SoundReset: true}); err == nil {
		t.Fatal("a sound plus sound_reset succeeded")
	}
	// A selector that matches nothing reports zero, not an error.
	none, err := manager.Update(ctx, alarm.UpdateIn{Time: "09:00", Sound: "birds"})
	if err != nil || none.Matched != 0 {
		t.Fatalf("no-match update = %+v, %v; want 0", none, err)
	}
	// An over-cap sound is refused before the row changes.
	if _, err := manager.Update(ctx, alarm.UpdateIn{ID: work.ID, Sound: "long"}); err == nil {
		t.Fatal("an over-cap sound was accepted")
	}
	// An unknown sound falls back to the default and reports it.
	unknown, err := manager.Update(ctx, alarm.UpdateIn{ID: work.ID, Sound: "cowbell"})
	if err != nil {
		t.Fatalf("unknown sound Update: %v", err)
	}
	if !unknown.SoundFallback || unknown.Alarm.Sound != "" {
		t.Fatalf("unknown update = %+v, want the default sound", unknown)
	}
}

// TestUpdateWithoutSelectorNeedsOneAlarm proves a bare override updates the
// only Alarm but is ambiguous with several.
func TestUpdateWithoutSelectorNeedsOneAlarm(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)

	if created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"}); err != nil {
		t.Fatalf("Set: %v", err)
	} else if created.Sound != "" {
		t.Fatalf("Set sound = %q, want none", created.Sound)
	}
	updated, err := manager.Update(ctx, alarm.UpdateIn{Sound: "birds"})
	if err != nil || updated.Matched != 1 || updated.Alarm.Sound != "birds" {
		t.Fatalf("single update = %+v, %v", updated, err)
	}
	if _, err := manager.Set(ctx, alarm.SetIn{Hour: 8, Schedule: "daily", Label: "late"}); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	ambiguous, err := manager.Update(ctx, alarm.UpdateIn{Sound: "rain"})
	if err != nil || !ambiguous.Ambiguous || ambiguous.Matched != 2 {
		t.Fatalf("two-alarm update = %+v, %v; want ambiguous", ambiguous, err)
	}
}

// TestUpdatePreservesNextRepeatAndSnooze proves the override write never
// touches next_fires_at or snoozed_until.
func TestUpdatePreservesNextRepeatAndSnooze(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)

	created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	snoozeTarget := clock.Now().Add(15 * time.Minute).UnixMilli()
	if _, err := fixture.store.DB().Exec(
		`UPDATE alarms SET snoozed_until = ? WHERE id = ?`, snoozeTarget, created.ID); err != nil {
		t.Fatalf("seed snooze: %v", err)
	}
	var beforeNext int64
	if err := fixture.store.DB().QueryRow(
		`SELECT next_fires_at FROM alarms WHERE id = ?`, created.ID).Scan(&beforeNext); err != nil {
		t.Fatalf("read next before: %v", err)
	}
	updated, err := manager.Update(ctx, alarm.UpdateIn{ID: created.ID, Sound: "birds", Volume: intPtr(35)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Alarm.Sound != "birds" || updated.Alarm.Volume == nil || *updated.Alarm.Volume != 35 {
		t.Fatalf("updated = %+v", updated)
	}
	var (
		afterNext   int64
		afterSnooze *int64
	)
	if err := fixture.store.DB().QueryRow(
		`SELECT next_fires_at, snoozed_until FROM alarms WHERE id = ?`, created.ID).
		Scan(&afterNext, &afterSnooze); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if afterNext != beforeNext {
		t.Fatalf("next_fires_at = %d, want %d", afterNext, beforeNext)
	}
	if afterSnooze == nil || *afterSnooze != snoozeTarget {
		t.Fatalf("snoozed_until = %v, want %d", afterSnooze, snoozeTarget)
	}
}

// TestUpdateSpeakReportsTheOutcome checks the two non-change replies.
func TestUpdateSpeakReportsTheOutcome(t *testing.T) {
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ambiguous, ok := reg.Speak(alarm.UpdateName, alarm.UpdateOut{Matched: 2, Ambiguous: true})
	if !ok || !strings.Contains(ambiguous, "More than one alarm") {
		t.Fatalf("ambiguous Speak = %q", ambiguous)
	}
	none, ok := reg.Speak(alarm.UpdateName, alarm.UpdateOut{Matched: 0})
	if !ok || !strings.Contains(none, "No alarm matched") {
		t.Fatalf("no-match Speak = %q", none)
	}
}

// TestUpdateRacingCancelDoesNotResurrect runs the store-backed race under the
// race detector. An UPDATE can never insert a row, so a completed cancel wins.
func TestUpdateRacingCancelDoesNotResurrect(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)

	for i := 0; i < 30; i++ {
		created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Minute: i % 60, Schedule: "daily"})
		if err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func(id string) {
			defer wg.Done()
			_, _ = manager.Update(ctx, alarm.UpdateIn{ID: id, Sound: "birds", Volume: intPtr(20)})
		}(created.ID)
		go func(id string) {
			defer wg.Done()
			_, _ = manager.Cancel(ctx, alarm.CancelIn{ID: id})
		}(created.ID)
		wg.Wait()

		var count int
		if err := fixture.store.DB().QueryRow(
			`SELECT COUNT(*) FROM alarms WHERE id = ?`, created.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", created.ID, err)
		}
		if count != 0 {
			t.Fatalf("alarm %s reappeared after cancel", created.ID)
		}
	}
}

// capturePlayer records every Ring pass and can end the Ring after one pass.
type capturePlayer struct {
	mu      sync.Mutex
	passes  []sound.Clip
	notify  chan sound.Clip
	onFirst func(ctx context.Context, pass int) error
}

func newCapturePlayer() *capturePlayer {
	return &capturePlayer{notify: make(chan sound.Clip, 8)}
}

func (p *capturePlayer) Play(ctx context.Context, clip sound.Clip) error {
	p.mu.Lock()
	p.passes = append(p.passes, clip)
	count := len(p.passes)
	p.mu.Unlock()
	p.notify <- clip
	if p.onFirst != nil {
		return p.onFirst(ctx, count)
	}
	return errors.New("captured")
}

func (p *capturePlayer) wait(t *testing.T) sound.Clip {
	t.Helper()
	select {
	case clip := <-p.notify:
		return clip
	case <-time.After(2 * time.Second):
		t.Fatal("the Alarm did not Ring")
		return sound.Clip{}
	}
}

// peakPCM returns the largest absolute sample in a mono PCM16 clip.
func peakPCM(pcm []byte) int16 {
	var peak int16
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	return peak
}

// overrideEngine builds the real Ring engine with the real per-Alarm sound
// library and a synthetic kind sound. It returns the capture player and the
// Focus registry so a test can snooze a live Ring.
func overrideEngine(cues *sound.Service, player *capturePlayer, boundary ring.Boundary) (*ring.Engine, *focus.Registry) {
	registry := focus.New()
	kindClip := sound.Clip{
		PCM:      make([]byte, 24),
		Format:   testFormat,
		Duration: 3 * time.Millisecond,
	}
	for i := 0; i < 12; i++ {
		binary.LittleEndian.PutUint16(kindClip.PCM[i*2:], uint16(20000))
	}
	engine := ring.New(ring.Deps{
		Focus:    registry,
		Boundary: boundary,
		Settings: func(ring.Kind) ring.Settings { return ring.Defaults(ring.Alarm) },
		Clip:     func(ring.Kind) (sound.Clip, bool) { return kindClip, true },
		ClipName: func(name string) (sound.Clip, bool) {
			return cues.ClipName(sound.RoleAlarmRing, name)
		},
		Play:   player.Play,
		Clock:  &fastClock{now: time.Now()},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return engine, registry
}

// TestPerAlarmRingClipGainRampOnlyForTheTargetedAlarm captures the actual PCM
// of two Alarms: the overridden one plays the birds clip at 40 % with no ramp,
// and the plain one keeps the 30 s kind ramp.
func TestPerAlarmRingClipGainRampOnlyForTheTargetedAlarm(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	player := newCapturePlayer()
	engine, _ := overrideEngine(fixture.cues, player, nil)
	manager.SetRingEngine(engine)

	if _, err := manager.Set(ctx, alarm.SetIn{
		Hour: 7, Sound: "birds", Volume: intPtr(40), RampSeconds: intPtr(0),
	}); err != nil {
		t.Fatalf("Set overridden: %v", err)
	}
	first := player.wait(t)
	birds, ok := fixture.cues.ClipName(sound.RoleAlarmRing, "birds")
	if !ok {
		t.Fatal("birds clip is missing from the library")
	}
	if len(first.PCM) != len(birds.PCM) {
		t.Fatalf("overridden pass has %d bytes, want the birds clip %d", len(first.PCM), len(birds.PCM))
	}
	if peak := peakPCM(first.PCM); peak < 3900 || peak > 4100 {
		t.Fatalf("overridden peak = %d, want 4000 (40%% of 10000)", peak)
	}

	// A second Alarm without overrides keeps the kind 30 s ramp and 80 %
	// volume; its first pass is almost silent, proving no leakage.
	if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7}); err != nil {
		t.Fatalf("Set plain: %v", err)
	}
	second := player.wait(t)
	if len(second.PCM) != 24 {
		t.Fatalf("plain pass has %d bytes, want the kind clip 24", len(second.PCM))
	}
	if peak := peakPCM(second.PCM); peak >= 100 {
		t.Fatalf("plain first-pass peak = %d, want the 30 s ramp to suppress it", peak)
	}
}

// TestPerAlarmOverridesSurviveRestart sets the overrides, closes the store
// before the fire, reopens it and captures the recovered Ring.
func TestPerAlarmOverridesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	soundsDir := filepath.Join(dir, "sounds")
	if err := os.MkdirAll(soundsDir, 0o700); err != nil {
		t.Fatalf("create sounds dir: %v", err)
	}
	writeSoundFile(t, soundsDir, "birds", 8, 10000)
	storePath := filepath.Join(dir, "assistant.db")
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}

	st := openStore(t, storePath)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cues, err := sound.New(ctx, st, dir, testFormat, logger)
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	names := newFakeMatcher(t)
	manager := alarm.NewManager(alarm.Options{Now: clock.Now, Logger: logger, Cues: cues, Names: names})
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	created, err := manager.Set(ctx, alarm.SetIn{
		Hour: 7, Sound: "birds", Volume: intPtr(25), RampSeconds: intPtr(0),
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	manager.Close()
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened := openStore(t, storePath)
	defer reopened.Close()
	cues, err = sound.New(ctx, reopened, dir, testFormat, logger)
	if err != nil {
		t.Fatalf("reopen sound.New: %v", err)
	}
	recovered := alarm.NewManager(alarm.Options{Now: clock.Now, Logger: logger, Cues: cues, Names: names})
	defer recovered.Close()
	// The row itself carries the overrides across the restart.
	var (
		savedSound  string
		savedVolume *int64
		savedRamp   *int64
	)
	if err := reopened.DB().QueryRow(
		`SELECT sound, volume, ramp_ms FROM alarms WHERE id = ?`, created.ID).
		Scan(&savedSound, &savedVolume, &savedRamp); err != nil {
		t.Fatalf("read persisted overrides: %v", err)
	}
	if savedSound != "birds" || savedVolume == nil || *savedVolume != 25 ||
		savedRamp == nil || *savedRamp != 0 {
		t.Fatalf("persisted overrides = %q/%v/%v, want birds/25/0", savedSound, savedVolume, savedRamp)
	}
	player := newCapturePlayer()
	engine, _ := overrideEngine(cues, player, nil)
	recovered.SetRingEngine(engine)
	if err := recovered.AttachStore(reopened, nil); err != nil {
		t.Fatalf("recover AttachStore: %v", err)
	}
	clip := player.wait(t)
	birds, ok := cues.ClipName(sound.RoleAlarmRing, "birds")
	if !ok || len(clip.PCM) != len(birds.PCM) {
		t.Fatalf("recovered pass is not the birds clip: %d bytes", len(clip.PCM))
	}
	if peak := peakPCM(clip.PCM); peak < 2400 || peak > 2600 {
		t.Fatalf("recovered peak = %d, want 2500 (25%%)", peak)
	}
}

// TestSnoozeKeepsPerAlarmOverrides snoozes the first Ring and captures the
// second, which must use the same clip, volume and ramp.
func TestSnoozeKeepsPerAlarmOverrides(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, func() time.Duration { return 50 * time.Millisecond })
	player := newCapturePlayer()
	engine, registry := overrideEngine(fixture.cues, player, nil)
	player.onFirst = func(ctx context.Context, pass int) error {
		if pass == 1 {
			// Snooze the live Ring exactly when it owns the speaker.
			registry.HandleIntent(context.Background(), focus.Snooze)
			<-ctx.Done()
			return ctx.Err()
		}
		return errors.New("captured")
	}
	manager.SetRingEngine(engine)
	if _, err := manager.Set(ctx, alarm.SetIn{
		Hour: 7, Sound: "birds", Volume: intPtr(25), RampSeconds: intPtr(0),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	first := player.wait(t)
	second := player.wait(t)
	birds, ok := fixture.cues.ClipName(sound.RoleAlarmRing, "birds")
	if !ok {
		t.Fatal("birds clip is missing")
	}
	for i, clip := range []sound.Clip{first, second} {
		if len(clip.PCM) != len(birds.PCM) {
			t.Fatalf("pass %d is not the birds clip", i+1)
		}
		if peak := peakPCM(clip.PCM); peak < 2400 || peak > 2600 {
			t.Fatalf("pass %d peak = %d, want 2500", i+1, peak)
		}
	}
}

// gatedBoundary holds a Ring at the reply-sentence seam.
type gatedBoundary struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedBoundary() *gatedBoundary {
	return &gatedBoundary{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *gatedBoundary) WaitForReplySentence(ctx context.Context) (func(), error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return func() {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestRingWaitsAtTheBoundaryThenUsesTheOverrides proves the per-Alarm settings
// are applied once the Ring gets the speaker, not before it can be observed.
func TestRingWaitsAtTheBoundaryThenUsesTheOverrides(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	player := newCapturePlayer()
	boundary := newGatedBoundary()
	engine, _ := overrideEngine(fixture.cues, player, boundary)
	manager.SetRingEngine(engine)
	if _, err := manager.Set(ctx, alarm.SetIn{
		Hour: 7, Sound: "birds", Volume: intPtr(40), RampSeconds: intPtr(0),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case <-boundary.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring never reached the boundary")
	}
	select {
	case clip := <-player.notify:
		t.Fatalf("the Ring played before the boundary released: %d bytes", len(clip.PCM))
	case <-time.After(50 * time.Millisecond):
	}
	close(boundary.release)
	clip := player.wait(t)
	birds, ok := fixture.cues.ClipName(sound.RoleAlarmRing, "birds")
	if !ok || len(clip.PCM) != len(birds.PCM) {
		t.Fatalf("post-boundary pass is not the birds clip")
	}
	if peak := peakPCM(clip.PCM); peak < 3900 || peak > 4100 {
		t.Fatalf("post-boundary peak = %d, want 4000", peak)
	}
}
