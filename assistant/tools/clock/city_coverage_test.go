package clock

import (
	"testing"
	"time"
)

// TestCityCoverageZonesLoad is the coverage bound check: every shipped alias
// names a real IANA zone and the table stays broad enough to be useful.
func TestCityCoverageZonesLoad(t *testing.T) {
	names := make(map[string]bool)
	zones := make(map[string]bool)
	for key, entry := range cities {
		if key == "" {
			t.Error("empty city key")
		}
		if entry.Name == "" || entry.Zone == "" {
			t.Errorf("city %q has empty name or zone: %+v", key, entry)
		}
		names[entry.Name] = true
		zones[entry.Zone] = true
		if _, err := time.LoadLocation(entry.Zone); err != nil {
			t.Errorf("city %q zone %q: %v", key, entry.Zone, err)
		}
	}
	t.Logf("city coverage: %d names in %d zones", len(names), len(zones))
	if len(names) < 140 {
		t.Errorf("coverage = %d named cities, want at least 140", len(names))
	}
	if len(zones) < 70 {
		t.Errorf("coverage = %d zones, want at least 70", len(zones))
	}
}

// TestAmbiguousCitiesAreNotAlsoBareEntries keeps the refusal table honest: a
// bare ambiguous name must not resolve through the main table by accident.
func TestAmbiguousCitiesAreNotAlsoBareEntries(t *testing.T) {
	for name := range ambiguousCities {
		if _, ok := cities[name]; ok {
			t.Errorf("ambiguous name %q also has a bare city entry", name)
		}
	}
}
