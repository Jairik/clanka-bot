package reply

import "testing"

// TestSentenceEndDistinguishesClausesFromSentences protects the Ring boundary:
// a clause ends with a comma or no punctuation and must not release the floor.
func TestSentenceEndDistinguishesClausesFromSentences(t *testing.T) {
	for _, test := range []struct {
		text string
		want bool
	}{
		{"Sure.", true},
		{"Bring a coat!", true},
		{"Is it ready?", true},
		{"She said \"go.\"", true},
		{"(Done.)", true},
		{"It depends on the time,", false},
		{"and the day", false},
		{"twelve words without punctuation", false},
		{"", false},
	} {
		if got := sentenceEnd(test.text); got != test.want {
			t.Errorf("sentenceEnd(%q) = %v, want %v", test.text, got, test.want)
		}
	}
}
