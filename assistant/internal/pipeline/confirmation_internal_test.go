package pipeline

import "testing"

// TestIsClearYesPinsTheConservativeYesSet covers every stored agreement, the
// punctuation STT and phone keyboards add around it, and the utterances that
// must still drop a pending call (#191).
func TestIsClearYesPinsTheConservativeYesSet(t *testing.T) {
	yes := []string{
		"yes",
		"Yes.",
		"yes!",
		"yeah",
		"yep",
		"yes please",
		"yes, please",
		"yes,please",
		"Yes, please.",
		"YES, PLEASE!",
		"  yes,   please  ",
		"sure",
		"ok",
		"okay.",
		"do it",
		"go ahead",
		"confirm",
		"affirmative",
		"please do",
	}
	for _, transcript := range yes {
		if !isClearYes(transcript) {
			t.Errorf("isClearYes(%q) = false, want true", transcript)
		}
	}

	no := []string{
		"",
		"   ",
		"no",
		"nope",
		"not now",
		"cancel",
		"stop",
		"yes no",
		"yes, thank you",
		"sure, later",
		"ok, but not really",
		"maybe",
	}
	for _, transcript := range no {
		if isClearYes(transcript) {
			t.Errorf("isClearYes(%q) = true, want false", transcript)
		}
	}
}
