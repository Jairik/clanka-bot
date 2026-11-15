package dashboard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
)

// TestSearchNoticeNamesTheLimit keeps the page honest about a partial search:
// a complete answer has no notice, and each windowed reason names the bound the
// store actually applied (#373 review, #210).
func TestSearchNoticeNamesTheLimit(t *testing.T) {
	if got := searchNotice(activity.SearchWindowNone); got != "" {
		t.Fatalf("a complete search showed the notice %q", got)
	}
	short := searchNotice(activity.SearchWindowShort)
	for _, want := range []string{fmt.Sprintf("newest %d", activity.ShortSearchWindow), "three characters"} {
		if !strings.Contains(short, want) {
			t.Errorf("short-query notice %q lacks %q", short, want)
		}
	}
	unavailable := searchNotice(activity.SearchWindowIndexUnavailable)
	for _, want := range []string{"index is unavailable", fmt.Sprintf("newest %d", activity.ShortSearchWindow)} {
		if !strings.Contains(unavailable, want) {
			t.Errorf("unavailable-index notice %q lacks %q", unavailable, want)
		}
	}
}
