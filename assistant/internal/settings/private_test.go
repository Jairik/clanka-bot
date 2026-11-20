package settings_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// TestPrivateSettingStaysOutOfTheUserList checks that a stored value is not a
// setting the Settings page can edit, and that a restart still reads it.
func TestPrivateSettingStaysOutOfTheUserList(t *testing.T) {
	svc, st, path := openSettings(t, settings.String("timezone", "UTC"))
	ctx := t.Context()
	const key = "lights.color.AA:BB:CC:DD:EE:FF"
	if err := svc.SetPrivate(ctx, key, "#ff8000"); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Get(key); ok {
		t.Fatal("Get exposed a private key")
	}
	for _, value := range svc.List() {
		if value.Key == key {
			t.Fatal("List exposed a private key")
		}
	}
	if _, err := svc.Set(ctx, key, "#ff8000"); err == nil {
		t.Fatal("Set accepted a private key")
	}
	if err := svc.SetPrivate(ctx, "timezone", "Europe/London"); err == nil {
		t.Fatal("SetPrivate overwrote a user setting")
	}

	reloaded, err := settings.New(ctx, st, settings.String("timezone", "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := reloaded.GetPrivate(ctx, key)
	if err != nil || !ok || got != "#ff8000" {
		t.Fatalf("reloaded %q ok=%v err=%v path=%s", got, ok, err, path)
	}
	if reloaded.String("timezone") != "UTC" {
		t.Fatal("private write changed a user setting")
	}
}
