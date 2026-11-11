package audio

import (
	"encoding/binary"
	"math"
)

// Chime tone shape: two short notes with a soft attack and release so the
// chime does not click on the speaker.
const (
	chimeAmplitude = 0.22
	chimeAttack    = 8e-3 // seconds
	chimeGap       = 20e-3
)

// chimeNotes are the two frequencies (Hz) and durations (seconds) of the
// built-in wake chime. They are deliberately short: the reply latency budget
// counts from the end of the question, but the wake chime is playing before
// the user speaks.
var chimeNotes = []struct {
	hz   float64
	secs float64
}{
	{880.0, 0.11},
	{1174.66, 0.14},
}

// Chime returns mono signed 16-bit little-endian PCM for the built-in wake
// chime at the given sample rate. The audio is generated rather than shipped
// as an asset, so it carries no licence and needs no file in the image.
func Chime(rate int) []byte {
	if rate <= 0 {
		return nil
	}
	samples := make([]int16, 0, int(float64(rate)*(0.11+0.14+chimeGap)))
	for _, note := range chimeNotes {
		count := int(note.secs * float64(rate))
		for i := 0; i < count; i++ {
			t := float64(i) / float64(rate)
			envelope := 1.0
			if t < chimeAttack {
				envelope = t / chimeAttack
			}
			if remaining := note.secs - t; remaining < chimeAttack {
				envelope = remaining / chimeAttack
			}
			value := chimeAmplitude * envelope * math.Sin(2*math.Pi*note.hz*t)
			samples = append(samples, int16(math.Round(value*math.MaxInt16)))
		}
		if note.hz != chimeNotes[len(chimeNotes)-1].hz {
			samples = append(samples, make([]int16, int(chimeGap*float64(rate)))...)
		}
	}

	pcm := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	return pcm
}
