package dashboard

import "testing"

// TestPathLabelBriefing keeps the Activity label for the morning-briefing stage
// in sync with the new PathBriefing value.
func TestPathLabelBriefing(t *testing.T) {
	if got := pathLabel("briefing"); got != "Briefing" {
		t.Errorf("pathLabel(%q) = %q, want %q", "briefing", got, "Briefing")
	}
}
