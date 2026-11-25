package voice

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

const testRate = 16000

// frame returns one 32 ms mono PCM16 frame at the given peak amplitude.
func frame(amplitude int16) []byte {
	samples := testRate * 32 / 1000
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(amplitude))
	}
	return pcm
}

func TestEndpointerWaitsForTheConfiguredSilence(t *testing.T) {
	tests := []struct {
		name     string
		silence  time.Duration
		quiet    int // 32 ms frames of silence after speech
		wantEnd  bool
		wantMore bool // the decision must not come before this many frames
	}{
		{name: "500ms window ends on the 16th quiet frame", silence: 500 * time.Millisecond, quiet: 16, wantEnd: true},
		{name: "500ms window is not cut short", silence: 500 * time.Millisecond, quiet: 15, wantEnd: false},
		{name: "300ms window ends sooner", silence: 300 * time.Millisecond, quiet: 10, wantEnd: true},
		{name: "300ms window is not cut short", silence: 300 * time.Millisecond, quiet: 9, wantEnd: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpointer, err := NewEndpointer(EndpointConfig{
				Rate:          testRate,
				Silence:       test.silence,
				ThresholdDBFS: -38,
			})
			if err != nil {
				t.Fatalf("NewEndpointer: %v", err)
			}
			endpointer.Add(frame(8000)) // speech
			ended := false
			for i := 0; i < test.quiet; i++ {
				ended = endpointer.Add(frame(0))
			}
			if ended != test.wantEnd {
				t.Fatalf("ended = %v after %d quiet frames (~%v), want %v", ended, test.quiet, time.Duration(test.quiet)*32*time.Millisecond, test.wantEnd)
			}
		})
	}
}

func TestEndpointerIgnoresBriefBlips(t *testing.T) {
	endpointer, err := NewEndpointer(EndpointConfig{
		Rate:          testRate,
		Silence:       500 * time.Millisecond,
		ThresholdDBFS: -38,
		MinSpeech:     200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewEndpointer: %v", err)
	}
	endpointer.Add(frame(8000)) // a chime tail or click
	for i := 0; i < 20; i++ {
		if endpointer.Add(frame(0)) {
			t.Fatal("a brief blip opened and ended an utterance")
		}
	}
	if endpointer.SpeechHeard() {
		t.Fatal("a single frame counted as the question")
	}

	for i := 0; i < 7; i++ { // 224 ms of speech
		endpointer.Add(frame(8000))
	}
	if !endpointer.SpeechHeard() {
		t.Fatal("sustained speech was not treated as the question")
	}
}

func TestEndpointerIgnoresSilenceBeforeSpeech(t *testing.T) {
	endpointer, err := NewEndpointer(EndpointConfig{Rate: testRate, Silence: 500 * time.Millisecond, ThresholdDBFS: -38})
	if err != nil {
		t.Fatalf("NewEndpointer: %v", err)
	}
	for i := 0; i < 100; i++ {
		if endpointer.Add(frame(0)) {
			t.Fatal("ended on silence with no speech")
		}
	}
	if endpointer.SpeechHeard() {
		t.Fatal("SpeechHeard = true without speech")
	}
}

func TestEndpointerResetsSilenceWhenSpeechResumes(t *testing.T) {
	endpointer, err := NewEndpointer(EndpointConfig{Rate: testRate, Silence: 500 * time.Millisecond, ThresholdDBFS: -38})
	if err != nil {
		t.Fatalf("NewEndpointer: %v", err)
	}
	endpointer.Add(frame(8000))
	for i := 0; i < 15; i++ {
		endpointer.Add(frame(0))
	}
	endpointer.Add(frame(8000))
	if endpointer.Add(frame(0)) {
		t.Fatal("ended immediately after speech resumed")
	}
	if endpointer.Silence() != 32*time.Millisecond {
		t.Fatalf("silence = %v, want one frame", endpointer.Silence())
	}
}

func TestEndpointerNoiseBelowThresholdIsNotSpeech(t *testing.T) {
	endpointer, err := NewEndpointer(EndpointConfig{Rate: testRate, Silence: 500 * time.Millisecond, ThresholdDBFS: -38})
	if err != nil {
		t.Fatalf("NewEndpointer: %v", err)
	}
	// -45 dBFS represents a calibrated source, below the -38 dBFS speech threshold.
	quiet := int16(math.Round(math.Pow(10, -45.0/20) * math.MaxInt16))
	for i := 0; i < 20; i++ {
		if endpointer.Add(frame(quiet)) {
			t.Fatal("ended on background noise")
		}
	}
	if endpointer.SpeechHeard() {
		t.Fatal("background noise counted as speech")
	}
}

func TestEndpointerCapsRunawaySessions(t *testing.T) {
	endpointer, err := NewEndpointer(EndpointConfig{
		Rate:          testRate,
		Silence:       500 * time.Millisecond,
		ThresholdDBFS: -38,
		MaxUtterance:  1 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewEndpointer: %v", err)
	}
	frames := 0
	for !endpointer.Add(frame(8000)) {
		frames++
		if frames > 100 {
			t.Fatal("endpointer never hit its cap")
		}
	}
	if frames < 30 || frames > 32 {
		t.Fatalf("ended after %d frames, want about 31", frames)
	}
}

func TestEndpointConfigIsValidated(t *testing.T) {
	if _, err := NewEndpointer(EndpointConfig{Rate: 0, Silence: time.Second}); err == nil {
		t.Fatal("accepted a zero rate")
	}
	if _, err := NewEndpointer(EndpointConfig{Rate: testRate, Silence: 0}); err == nil {
		t.Fatal("accepted a zero silence window")
	}
	if _, err := NewEndpointer(EndpointConfig{Rate: testRate, Silence: time.Second, MaxUtterance: time.Millisecond}); err == nil {
		t.Fatal("accepted a cap shorter than the silence window")
	}
}

// TestPrerollFrameCountIsAFixedBound pins the bounded pre-roll from #213: it is
// derived only from the frame size and sample rate, never the utterance length,
// and is about 500 ms (above MinSpeech's 200 ms) so the first word is not
// clipped.
func TestPrerollFrameCountIsAFixedBound(t *testing.T) {
	// Production frame size: 1024 bytes at 16 kHz mono is 32 ms, so 500 ms is
	// 16 frames.
	if got := PrerollFrameCount(1024, 16000); got != 16 {
		t.Fatalf("PrerollFrameCount(1024, 16000) = %d, want 16 (~512 ms)", got)
	}
	// The bound tracks the frame size, not the utterance: doubling the frame
	// size halves the frame count for the same 500 ms window.
	if got := PrerollFrameCount(2048, 16000); got != 8 {
		t.Fatalf("PrerollFrameCount(2048, 16000) = %d, want 8 (~512 ms)", got)
	}
	// A degenerate rate yields a safe zero bound rather than a panic.
	if got := PrerollFrameCount(1024, 0); got != 0 {
		t.Fatalf("PrerollFrameCount with zero rate = %d, want 0", got)
	}
}

// TestLastSpeechIsTheLastVoicedFrame pins Response latency's start point: the
// most recent frame at or above the threshold, not the end of the silence
// window that fires the endpointer.
func TestLastSpeechIsTheLastVoicedFrame(t *testing.T) {
	endpointer, err := NewEndpointer(EndpointConfig{
		Rate:          testRate,
		Silence:       160 * time.Millisecond,
		ThresholdDBFS: -38,
		MinSpeech:     32 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewEndpointer: %v", err)
	}
	clock := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	endpointer.now = func() time.Time { return clock }

	clock = clock.Add(32 * time.Millisecond)
	endpointer.Add(frame(8000))
	clock = clock.Add(32 * time.Millisecond)
	endpointer.Add(frame(8000))
	voicedAt := clock

	fired := false
	for i := 0; i < 5 && !fired; i++ {
		clock = clock.Add(32 * time.Millisecond)
		fired = endpointer.Add(frame(0))
	}

	if !fired {
		t.Fatal("endpointer did not fire after the silence window")
	}
	if !endpointer.LastSpeech().Equal(voicedAt) {
		t.Fatalf("last speech = %s, want %s", endpointer.LastSpeech(), voicedAt)
	}
	if silence := clock.Sub(endpointer.LastSpeech()); silence < 160*time.Millisecond {
		t.Fatalf("silence since last speech = %s, want at least the window", silence)
	}
}

type fakeVADSession struct {
	scores []float64
	errAt  int
	calls  int
}

func (f *fakeVADSession) Score([]byte) (float64, error) {
	idx := f.calls
	f.calls++
	if f.errAt > 0 && f.calls >= f.errAt {
		return 0, errFakeVAD
	}
	if idx < len(f.scores) {
		return f.scores[idx], nil
	}
	return 0, nil
}

func (f *fakeVADSession) Close() error { return nil }

var errFakeVAD = &fakeVADError{}

type fakeVADError struct{}

func (*fakeVADError) Error() string { return "vad disconnected" }

func TestVADRejectsLoudRoomNoiseAndAppliesHysteresis(t *testing.T) {
	// Loud frames (well above -38 dBFS RMS) that Silero classifies as non-speech
	// must NOT open an utterance, while speech above 0.45 opens it and stays
	// active while probability hovers in the hysteresis band [0.30, 0.45).
	vad := &fakeVADSession{
		scores: []float64{
			0.05, 0.10, 0.08, // loud room noise -> rejected
			0.85, 0.70, // speech onset (>= 0.45)
			0.35, 0.32, // trailing consonant in hysteresis band (>= 0.30) -> still speech
			0.10, 0.05, 0.02, 0.01, 0.01, // 5 quiet frames (160 ms) -> ends
		},
	}
	ep, err := NewEndpointerWithDetector(EndpointConfig{
		Rate:          testRate,
		VAD:           true,
		VADThreshold:  0.45,
		Silence:       160 * time.Millisecond,
		ThresholdDBFS: -38,
		MinSpeech:     64 * time.Millisecond,
	}, NewVADDetector(vad, 0.45), nil)
	if err != nil {
		t.Fatalf("NewEndpointerWithDetector: %v", err)
	}
	if !ep.UsingVAD() {
		t.Fatal("UsingVAD = false, want true")
	}

	// 3 loud room-noise frames: RMS would trigger, VAD rejects them.
	for i := 0; i < 3; i++ {
		if ep.Add(frame(8000)) {
			t.Fatalf("ended during room noise frame %d", i)
		}
	}
	if ep.SpeechHeard() {
		t.Fatal("loud room noise triggered SpeechHeard despite low VAD scores")
	}

	// 2 speech onset frames + 2 hysteresis frames.
	for i := 0; i < 4; i++ {
		if ep.Add(frame(8000)) {
			t.Fatalf("ended during active speech frame %d", i)
		}
	}
	if !ep.SpeechHeard() {
		t.Fatal("SpeechHeard = false after voiced frames")
	}
	if ep.Silence() != 0 {
		t.Fatalf("silence = %v during hysteresis band, want 0", ep.Silence())
	}

	// 5 non-speech frames (still loud room noise!) end the utterance after 160 ms.
	ended := false
	for i := 0; i < 5; i++ {
		ended = ep.Add(frame(8000))
	}
	if !ended {
		t.Fatal("utterance did not end after 160 ms of VAD non-speech")
	}
}

func TestEndpointerFallsBackToRMSWhenVADErrorsMidUtterance(t *testing.T) {
	vad := &fakeVADSession{
		scores: []float64{0.90, 0.85},
		errAt:  3,
	}
	var fallbackErr error
	ep, err := NewEndpointerWithDetector(EndpointConfig{
		Rate:          testRate,
		VAD:           true,
		VADThreshold:  0.45,
		Silence:       128 * time.Millisecond,
		ThresholdDBFS: -38,
		MinSpeech:     32 * time.Millisecond,
	}, NewVADDetector(vad, 0.45), func(e error) { fallbackErr = e })
	if err != nil {
		t.Fatalf("NewEndpointerWithDetector: %v", err)
	}

	ep.Add(frame(8000))
	ep.Add(frame(8000))
	if !ep.UsingVAD() {
		t.Fatal("UsingVAD = false before error")
	}
	// Third frame triggers VAD error -> falls back to RMS for the rest of the turn.
	ended := false
	for i := 0; i < 4 && !ended; i++ {
		ended = ep.Add(frame(0))
	}
	if fallbackErr == nil {
		t.Fatal("onFallback was not invoked")
	}
	if ep.UsingVAD() {
		t.Fatal("UsingVAD = true after fallback")
	}
	if !ended {
		t.Fatal("RMS fallback did not end the utterance on silence")
	}
	// Once failed, VAD is not called again for subsequent frames.
	if vad.calls != 3 {
		t.Fatalf("vad.calls = %d, want 3 (no retry after failure)", vad.calls)
	}
}
