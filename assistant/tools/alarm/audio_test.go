package alarm_test

import (
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
	"testing"
)

// Completion titles cannot supply the clock or override label in alarm.set.
func TestCompletionAudioExtraction(t *testing.T) {
	in, ok := alarm.NewSet(nil).Extract("set an alarm for 7 am and play 99 Luftballons")
	if !ok || in.Hour != 7 || in.Minute != 0 || in.Audio != "99 Luftballons" {
		t.Fatalf("input=%+v ok=%v", in, ok)
	}
}
