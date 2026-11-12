package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// warmTTS is a minimal Wyoming server that records every voice-warm request.
type warmTTS struct {
	addr string
	ln   net.Listener
	wg   sync.WaitGroup

	mu   sync.Mutex
	refs []string
}

func newWarmTTS(t *testing.T) *warmTTS {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &warmTTS{addr: listener.Addr().String(), ln: listener}
	t.Cleanup(func() {
		_ = listener.Close()
		f.wg.Wait()
	})
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var frame struct {
						Type          string         `json:"type"`
						Data          map[string]any `json:"data"`
						PayloadLength int            `json:"payload_length"`
					}
					if err := json.Unmarshal(line, &frame); err != nil {
						return
					}
					if frame.PayloadLength > 0 {
						if _, err := reader.Discard(frame.PayloadLength); err != nil {
							return
						}
					}
					if frame.Type != "voice-warm" {
						continue
					}
					ref, _ := frame.Data["voice"].(string)
					f.mu.Lock()
					f.refs = append(f.refs, ref)
					f.mu.Unlock()
					if _, err := conn.Write([]byte(`{"type":"voice-warmed","data":{"ok":true,"error":""}}` + "\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	return f
}

func (f *warmTTS) warmed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refs...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// warmFixture wires a real voice library, settings service and Persona service.
func warmFixture(t *testing.T, voicesPersona persona.VoiceLibrary) (*voicelib.Service, *settings.Service, *persona.Service) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	voices, err := voicelib.New(ctx, st, voicelib.Config{
		Dir:      filepath.Join(dir, "voices"),
		TTSDir:   "/voices",
		Default:  "alba",
		Builtins: []string{"alba", "jarvis"},
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	if voicesPersona == nil {
		voicesPersona = voices
	}
	personas, err := persona.New(ctx, st, voicesPersona, discardLogger())
	if err != nil {
		t.Fatalf("persona.New: %v", err)
	}
	defs := []settings.Definition{
		settings.String(voicelib.KeyName, "alba", settings.WithConfig("alba")),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig(wording.DefaultFiller)),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage, settings.WithConfig(wording.DefaultUnavailableMessage)),
	}
	defs = append(defs, personas.Definitions()...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	voices.Bind(svc)
	personas.Bind(svc)
	return voices, svc, personas
}

func TestWarmStartupWarmsTheActiveVoiceAndTheActivePersonaVoice(t *testing.T) {
	voices, settingsSvc, personas := warmFixture(t, nil)
	ctx := context.Background()
	if _, err := personas.Create(ctx, persona.Input{
		Name: "GLaDOS", Voice: "jarvis", Personality: "You are GLaDOS.",
		Filler: "Processing.", Unavailable: "Offline.",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := personas.Select(ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	// A manual override moves the speaking voice away from the Persona voice;
	// startup must still warm both refs.
	if _, err := settingsSvc.Set(ctx, voicelib.KeyName, "alba"); err != nil {
		t.Fatalf("override voice: %v", err)
	}

	fake := newWarmTTS(t)
	client := wyoming.NewClient(fake.addr, wyoming.WithTimeout(2*time.Second))
	warmStartupVoices(ctx, discardLogger(), client, voices, personas, 2*time.Second)

	got := fake.warmed()
	if len(got) != 2 || got[0] != "alba" || got[1] != "jarvis" {
		t.Fatalf("warmed refs = %v, want [alba jarvis]", got)
	}
}

func TestWarmStartupDeduplicatesTheSameVoice(t *testing.T) {
	voices, _, personas := warmFixture(t, nil)
	ctx := context.Background()
	if _, err := personas.Create(ctx, persona.Input{
		Name: "GLaDOS", Voice: "jarvis", Personality: "You are GLaDOS.",
		Filler: "Processing.", Unavailable: "Offline.",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := personas.Select(ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}

	fake := newWarmTTS(t)
	client := wyoming.NewClient(fake.addr, wyoming.WithTimeout(2*time.Second))
	warmStartupVoices(ctx, discardLogger(), client, voices, personas, 2*time.Second)
	if got := fake.warmed(); len(got) != 1 || got[0] != "jarvis" {
		t.Fatalf("warmed refs = %v, want one jarvis", got)
	}
}

func TestWarmStartupSkipsAStalePersonaVoiceWithoutFailing(t *testing.T) {
	stale := &staleVoiceLibrary{
		entries: map[string]voicelib.Voice{
			"alba":   {Name: "alba", Builtin: true},
			"glados": {Name: "glados", ValidatedRevision: "rev-1"},
		},
		active: "alba",
	}
	voices, settingsSvc, personas := warmFixture(t, stale)
	ctx := context.Background()
	if _, err := personas.Create(ctx, persona.Input{
		Name: "GLaDOS", Voice: "glados", Personality: "You are GLaDOS.",
		Filler: "Processing.", Unavailable: "Offline.",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := personas.Select(ctx, "GLaDOS"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	// The speaking voice is manually made safe; the stored Persona voice is
	// stale. Startup warms the safe voice, logs and skips the other, and does
	// not fail or queue a second announcement.
	if _, err := settingsSvc.Set(ctx, voicelib.KeyName, "alba"); err != nil {
		t.Fatalf("override voice: %v", err)
	}
	stale.entries["glados"] = voicelib.Voice{Name: "glados", ValidatedRevision: "rev-1", Stale: true}

	fake := newWarmTTS(t)
	client := wyoming.NewClient(fake.addr, wyoming.WithTimeout(2*time.Second))
	warmStartupVoices(ctx, discardLogger(), client, voices, personas, 2*time.Second)
	if got := fake.warmed(); len(got) != 1 || got[0] != "alba" {
		t.Fatalf("warmed refs = %v, want only the safe active voice", got)
	}
}

// staleVoiceLibrary exposes a readiness change without a TTS revision change.
type staleVoiceLibrary struct {
	entries map[string]voicelib.Voice
	active  string
}

func (s *staleVoiceLibrary) Get(name string) (voicelib.Voice, bool) {
	for key, voice := range s.entries {
		if strings.EqualFold(key, name) {
			return voice, true
		}
	}
	return voicelib.Voice{}, false
}

func (s *staleVoiceLibrary) Active() (voicelib.Voice, bool) { return s.Get(s.active) }

func (s *staleVoiceLibrary) DefaultName() string { return "alba" }

func (s *staleVoiceLibrary) SpeechRef(name string) (string, error) {
	voice, ok := s.Get(name)
	if !ok {
		return "", voicelib.ErrUnknownVoice
	}
	if voice.Stale {
		return "", voicelib.ErrStaleVoice
	}
	if !voice.Ready() {
		return "", voicelib.ErrNotValidated
	}
	return voice.Name, nil
}
