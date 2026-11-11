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
	// -45 dBFS is roughly the measured noise floor of the dev box mic.
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
