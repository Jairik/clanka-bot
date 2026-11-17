package speech_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// TestPronouncerWordBoundaries proves substitution matches on word boundaries only:
// "Pi" is not replaced inside "Pizza", and "cat" is not replaced inside "category".
func TestPronouncerWordBoundaries(t *testing.T) {
	p := speech.NewPronouncer([]string{
		"Pi=pie",
		"cat=feline",
	})

	cases := []struct {
		input string
		want  string
	}{
		{"The Pi is fast", "The pie is fast"},
		{"Pi-powered device", "pie-powered device"},
		{"Hello, Pi!", "Hello, pie!"},
		{"Pizza is delicious", "Pizza is delicious"},
		{"Raspbian on a Pi", "Raspbian on a pie"},
		{"magpie in the tree", "magpie in the tree"},
		{"The cat sat on the mat", "The feline sat on the mat"},
		{"category theory", "category theory"},
		{"bobcat in the wild", "bobcat in the wild"},
		{"cat, dog and mouse", "feline, dog and mouse"},
	}

	for _, tc := range cases {
		if got := p.Replace(tc.input); got != tc.want {
			t.Errorf("Replace(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestPronouncerCaseInsensitivity proves that lookup ignores case while matching.
func TestPronouncerCaseInsensitivity(t *testing.T) {
	p := speech.NewPronouncer([]string{
		"SearXNG=search engine",
	})

	cases := []struct {
		input string
		want  string
	}{
		{"SearXNG", "search engine"},
		{"searxng", "search engine"},
		{"SEARXNG", "search engine"},
		{"sEArXng", "search engine"},
		{"SearXNG is running", "search engine is running"},
		{"SEARXNG is running", "search engine is running"},
	}

	for _, tc := range cases {
		if got := p.Replace(tc.input); got != tc.want {
			t.Errorf("Replace(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestPronouncerLongestKeyFirst proves that when two keys share a prefix,
// the longer key takes precedence regardless of entry order.
func TestPronouncerLongestKeyFirst(t *testing.T) {
	// Provide the shorter key first to verify sorting.
	p := speech.NewPronouncer([]string{
		"Sear=burn",
		"SearXNG=search engine",
	})

	if got := p.Replace("SearXNG is fast"); got != "search engine is fast" {
		t.Fatalf("Replace(SearXNG) = %q, want %q", got, "search engine is fast")
	}
	if got := p.Replace("Sear the meat"); got != "burn the meat" {
		t.Fatalf("Replace(Sear) = %q, want %q", got, "burn the meat")
	}
}

// TestPronouncerPunctuationAdjacency proves that words adjacent to various punctuation
// characters are recognized as word boundaries.
func TestPronouncerPunctuationAdjacency(t *testing.T) {
	p := speech.NewPronouncer([]string{
		"cat=feline",
	})

	cases := []struct {
		input string
		want  string
	}{
		{"(cat)", "(feline)"},
		{"cat.", "feline."},
		{"cat!", "feline!"},
		{"cat?", "feline?"},
		{"\"cat\"", "\"feline\""},
		{"'cat'", "'feline'"},
		{"cat:", "feline:"},
		{"cat;", "feline;"},
		{"cat, dog", "feline, dog"},
		{"cat/dog", "feline/dog"},
		{"[cat]", "[feline]"},
		{"{cat}", "{feline}"},
	}

	for _, tc := range cases {
		if got := p.Replace(tc.input); got != tc.want {
			t.Errorf("Replace(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestPronouncerUnicode proves that Unicode words and letters (accented, Cyrillic)
// are handled safely with correct boundary and case behavior.
func TestPronouncerUnicode(t *testing.T) {
	p := speech.NewPronouncer([]string{
		"Кланка=робот",
		"Piña=pineapple",
		"Pi=pie",
	})

	cases := []struct {
		input string
		want  string
	}{
		{"Привет, Кланка!", "Привет, робот!"},
		{"Привет, кланка!", "Привет, робот!"},
		{"Кланкабот здесь", "Кланкабот здесь"}, // 'б' is a letter, so no boundary
		{"Piña colada", "pineapple colada"},
		{"Piña", "pineapple"},
		{"The Pi is here", "The pie is here"},
	}

	for _, tc := range cases {
		if got := p.Replace(tc.input); got != tc.want {
			t.Errorf("Replace(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestPronouncerEmptyAndMalformedLines proves that blank, malformed, or oversized
// lines are safely ignored at build time without breaking valid entries.
func TestPronouncerEmptyAndMalformedLines(t *testing.T) {
	p := speech.NewPronouncer([]string{
		"",
		"   ",
		"no_equals_line",
		"=missing_key",
		"   =missing_key_spaces",
		strings.Repeat("k", 61) + "=too_long_key",
		"too_long_spoken=" + strings.Repeat("v", 61),
		"valid=good",
	})

	if got := p.Replace("valid"); got != "good" {
		t.Fatalf("Replace(valid) = %q, want %q", got, "good")
	}
	if got := p.Replace("no_equals_line"); got != "no_equals_line" {
		t.Fatalf("Replace(no_equals_line) = %q, want unchanged", got)
	}
}

// TestPronouncer200EntryCap proves that the pronouncer caps valid entries at 200.
func TestPronouncer200EntryCap(t *testing.T) {
	lines := make([]string, 0, 250)
	for i := 0; i < 250; i++ {
		lines = append(lines, fmt.Sprintf("word%03d=spoken%03d", i, i))
	}

	p := speech.NewPronouncer(lines)

	// First 200 (word000 through word199) should be active.
	if got := p.Replace("word000"); got != "spoken000" {
		t.Fatalf("Replace(word000) = %q, want spoken000", got)
	}
	if got := p.Replace("word199"); got != "spoken199" {
		t.Fatalf("Replace(word199) = %q, want spoken199", got)
	}

	// 201st entry (word200) and later should be ignored.
	if got := p.Replace("word200"); got != "word200" {
		t.Fatalf("Replace(word200) = %q, want word200 (capped)", got)
	}
}

// TestSettingsPronunciationOverrideConfigAndValidation proves that a stored override
// wins over config.yaml, Reset returns to config.yaml, and invalid lines are rejected.
func TestSettingsPronunciationOverrideConfigAndValidation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	configVal := []string{"SearXNG=search engine"}
	def := speech.PronunciationDefinition(configVal)
	svc, err := settings.New(ctx, st, def)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}

	// 1. Config layer applies initially.
	val, ok := svc.Get(speech.PronunciationKey)
	if !ok {
		t.Fatal("speech.pronunciation not found")
	}
	if val.Source != settings.SourceConfig {
		t.Fatalf("source = %v, want config", val.Source)
	}
	lines := svc.Strings(speech.PronunciationKey)
	if len(lines) != 1 || lines[0] != "SearXNG=search engine" {
		t.Fatalf("Strings = %+v, want config value", lines)
	}

	// 2. Stored override wins over config.yaml.
	overrideVal := []string{"SearXNG=search engine two", "ntfy=notify"}
	if _, err := svc.Set(ctx, speech.PronunciationKey, overrideVal); err != nil {
		t.Fatalf("svc.Set override: %v", err)
	}
	val, _ = svc.Get(speech.PronunciationKey)
	if val.Source != settings.SourceOverride {
		t.Fatalf("source after Set = %v, want override", val.Source)
	}
	lines = svc.Strings(speech.PronunciationKey)
	if len(lines) != 2 || lines[0] != "SearXNG=search engine two" {
		t.Fatalf("Strings after Set = %+v, want override values", lines)
	}

	// 3. Reset returns to config.yaml.
	if _, err := svc.Reset(ctx, speech.PronunciationKey); err != nil {
		t.Fatalf("svc.Reset: %v", err)
	}
	val, _ = svc.Get(speech.PronunciationKey)
	if val.Source != settings.SourceConfig {
		t.Fatalf("source after Reset = %v, want config", val.Source)
	}
	lines = svc.Strings(speech.PronunciationKey)
	if len(lines) != 1 || lines[0] != "SearXNG=search engine" {
		t.Fatalf("Strings after Reset = %+v, want config value", lines)
	}

	// 4. Invalid lines are refused at write time.
	badInputs := [][]string{
		{"no_equals"},
		{"=empty_key"},
		{"  =empty_key_spaces"},
		{strings.Repeat("a", 61) + "=valid"},
		{"valid=" + strings.Repeat("b", 61)},
	}
	for _, bad := range badInputs {
		if _, err := svc.Set(ctx, speech.PronunciationKey, bad); err == nil {
			t.Errorf("Set(%+v) succeeded, want error", bad)
		}
	}

	// 201 entries exceeds the 200 cap and must be refused.
	tooMany := make([]string, 201)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("k%03d=v%03d", i, i)
	}
	if _, err := svc.Set(ctx, speech.PronunciationKey, tooMany); err == nil {
		t.Errorf("Set with 201 entries succeeded, want error")
	}

	// Blank lines within valid entries are allowed and skipped.
	withBlanks := []string{"", "  ", "k1=v1", "", "k2=v2"}
	if _, err := svc.Set(ctx, speech.PronunciationKey, withBlanks); err != nil {
		t.Fatalf("Set with blank lines failed: %v", err)
	}
}

// TestSpeakerPronunciationAppliesWithoutRestart proves that updating the bound
// pronunciation function applies to the next Say call without recreating the Speaker.
func TestSpeakerPronunciationAppliesWithoutRestart(t *testing.T) {
	requests := make(chan string, 2)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(conn)
			line, err := r.ReadString('\n')
			if err != nil {
				conn.Close()
				return
			}
			var req struct {
				Type string         `json:"type"`
				Data map[string]any `json:"data"`
			}
			_ = json.Unmarshal([]byte(line), &req)
			if txt, ok := req.Data["text"].(string); ok {
				requests <- txt
			}
			writeEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
			writeAudio(t, conn, []byte{1, 2})
			writeEvent(t, conn, "audio-stop", nil)
			conn.Close()
		}
	}()

	var mu sync.RWMutex
	var currentPronouncer *speech.Pronouncer

	speaker := speech.New(wyoming.NewClient(ln.Addr().String()), newRecorder(), wyoming.Voice{Name: "alba"}, nil)
	speaker.BindPronunciation(func() *speech.Pronouncer {
		mu.RLock()
		defer mu.RUnlock()
		return currentPronouncer
	})

	// 1. Initial Say with no pronouncer transmits raw text.
	if _, err := speaker.Say(context.Background(), "SearXNG is ready"); err != nil {
		t.Fatalf("first Say: %v", err)
	}
	select {
	case req := <-requests:
		if req != "SearXNG is ready" {
			t.Fatalf("first synthesize request = %q, want raw text", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first synthesize request")
	}

	// 2. Install pronouncer live without restarting speaker.
	mu.Lock()
	currentPronouncer = speech.NewPronouncer([]string{"SearXNG=search engine"})
	mu.Unlock()

	// 3. Next Say immediately applies substitution.
	if _, err := speaker.Say(context.Background(), "SearXNG is ready"); err != nil {
		t.Fatalf("second Say: %v", err)
	}
	select {
	case req := <-requests:
		if req != "search engine is ready" {
			t.Fatalf("second synthesize request = %q, want substituted text", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for second synthesize request")
	}
}

// TestTTSCacheKeysOnSubstitutedText proves that the TTS cache key uses the
// substituted text, so a pronunciation change misses the cache rather than replaying old audio.
func TestTTSCacheKeysOnSubstitutedText(t *testing.T) {
	var synthCalls int32
	var lastSynthesized atomic.Pointer[string]

	addr, count := serveTTS(t, func(c net.Conn) {
		r := bufio.NewReader(c)
		line, _ := r.ReadString('\n')
		var req struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		if txt, ok := req.Data["text"].(string); ok {
			lastSynthesized.Store(&txt)
		}
		atomic.AddInt32(&synthCalls, 1)
		writeEvent(t, c, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, c, []byte{1, 2, 3, 4})
		writeEvent(t, c, "audio-stop", nil)
	})

	var mu sync.RWMutex
	var pronouncer *speech.Pronouncer

	sp := speech.New(wyoming.NewClient(addr), newRecorder(), wyoming.Voice{Name: "alba"}, nil)
	sp.BindPronunciation(func() *speech.Pronouncer {
		mu.RLock()
		defer mu.RUnlock()
		return pronouncer
	})

	ctx := context.Background()

	// 1. Initial Say with no pronouncer: synthesizes "SearXNG".
	if _, err := sp.Say(ctx, "SearXNG"); err != nil {
		t.Fatalf("Say 1: %v", err)
	}
	if count() != 1 {
		t.Fatalf("synthesis count = %d, want 1", count())
	}
	if last := lastSynthesized.Load(); last == nil || *last != "SearXNG" {
		t.Fatalf("synthesized text = %v, want SearXNG", last)
	}

	// 2. Repeat exact phrase: hits the PCM cache, no TTS request.
	if _, err := sp.Say(ctx, "SearXNG"); err != nil {
		t.Fatalf("Say 2: %v", err)
	}
	if count() != 1 {
		t.Fatalf("synthesis count = %d, want 1 (cache hit)", count())
	}

	// 3. Live pronunciation map added: SearXNG -> search engine.
	mu.Lock()
	pronouncer = speech.NewPronouncer([]string{"SearXNG=search engine"})
	mu.Unlock()

	// 4. Same input text "SearXNG": substituted to "search engine", which misses cache!
	if _, err := sp.Say(ctx, "SearXNG"); err != nil {
		t.Fatalf("Say 3: %v", err)
	}
	if count() != 2 {
		t.Fatalf("synthesis count = %d, want 2 (cache miss on pronunciation change)", count())
	}
	if last := lastSynthesized.Load(); last == nil || *last != "search engine" {
		t.Fatalf("synthesized text = %v, want search engine", last)
	}

	// 5. Repeat again: hits the cache for "search engine".
	if _, err := sp.Say(ctx, "SearXNG"); err != nil {
		t.Fatalf("Say 4: %v", err)
	}
	if count() != 2 {
		t.Fatalf("synthesis count = %d, want 2 (cache hit for new pronunciation)", count())
	}
}
