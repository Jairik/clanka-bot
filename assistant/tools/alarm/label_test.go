package alarm

import "testing"

// TestLabelForAlarm covers the natural label forms and the non-labels.
func TestLabelForAlarm(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"set a work alarm for 7:30", "work"},
		{"set an alarm called gym for 6:30", "gym"},
		{"cancel my work alarm", "work"},
		{"wake me at 7", ""},
		{"cancel my 7 am alarm", ""},
		{"cancel my wake up alarm", ""},
		{"delete my 8 o'clock alarm", ""},
		{"set an alarm for 7", ""},
	}
	for _, tc := range cases {
		if got := labelForAlarm(tc.text); got != tc.want {
			t.Errorf("labelForAlarm(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
