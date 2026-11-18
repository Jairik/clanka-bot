package lighting_test

import (
	"errors"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/lighting"
)

// Addressed separates "this transport has no lights" (skip it) from "it owns
// lights but failed" (say so), which is what the group reply depends on.
func TestAddressed(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  lighting.Result
		want bool
	}{
		{"no account", lighting.Result{}, false},
		{"lights present", lighting.Result{Total: 1}, true},
		{"failure while finding them", lighting.Result{Err: errors.New("down")}, true},
		{"partial batch", lighting.Result{Applied: 1, Total: 2}, true},
	} {
		if got := tc.res.Addressed(); got != tc.want {
			t.Errorf("%s: Addressed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
