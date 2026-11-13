package note

import "testing"

func TestExtractRemember(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"remember that I parked on level 3", "I parked on level 3"},
		{"Remember I left my bike in the shed.", "I left my bike in the shed"},
		{"make a note that the boiler needs servicing", "the boiler needs servicing"},
		{"add a note that the spare key is under the pot", "the spare key is under the pot"},
		{"note down that the plumber is coming Friday", "the plumber is coming Friday"},
		{"please remember that I owe Sam ten pounds", "I owe Sam ten pounds"},
	}
	for _, tc := range cases {
		got, ok := extractRemember(tc.text)
		if !ok || got.Text != tc.want {
			t.Errorf("extractRemember(%q) = %+v, %v; want %q", tc.text, got, ok, tc.want)
		}
	}
	for _, text := range []string{
		"",
		"what do I remember about the boiler",
		"where did I park",
		"remember my phone number",
		"remind me to take the bins out in ten minutes",
		"remember to call the dentist",
		"add milk to the shopping list",
	} {
		if got, ok := extractRemember(text); ok {
			t.Errorf("extractRemember(%q) = %+v, want no match", text, got)
		}
	}
}

func TestExtractRecall(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"what do I remember about the boiler", "the boiler"},
		{"what did I note about the plumber?", "the plumber"},
		{"do I have a note about the spare key", "the spare key"},
		{"recall my note about the car insurance", "the car insurance"},
		{"search my notes for the wifi password", "the wifi password"},
		{"what do I know about the library books", "the library books"},
		{"what have I noted about the bins", "the bins"},
		{"find my note about the passport", "the passport"},
		{"where did I park", "where did I park"},
		{"where did I put the passport", "the passport"},
	}
	for _, tc := range cases {
		got, ok := extractRecall(tc.text)
		if !ok || got.Query != tc.want {
			t.Errorf("extractRecall(%q) = %+v, %v; want %q", tc.text, got, ok, tc.want)
		}
	}
	for _, text := range []string{
		"",
		"what reminders do I have",
		"what's on my shopping list",
		"what do I have in my notes app",
		"remember that I parked on level 3",
		"what time is it",
	} {
		if got, ok := extractRecall(text); ok {
			t.Errorf("extractRecall(%q) = %+v, want no match", text, got)
		}
	}
}
