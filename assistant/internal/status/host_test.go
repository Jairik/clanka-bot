package status_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	_ "modernc.org/sqlite"
)

// fakeVitalsFiles returns canned sysfs/procfs files for vitals testing.
func fakeVitalsFiles(temp string, load string, mem string, uptime string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		switch path {
		case status.ThermalZonePath:
			if temp == "" {
				return nil, os.ErrNotExist
			}
			return []byte(temp), nil
		case status.LoadAvgPath:
			if load == "" {
				return nil, os.ErrNotExist
			}
			return []byte(load), nil
		case status.MemInfoPath:
			if mem == "" {
				return nil, os.ErrNotExist
			}
			return []byte(mem), nil
		case status.UptimePath:
			if uptime == "" {
				return nil, os.ErrNotExist
			}
			return []byte(uptime), nil
		default:
			return nil, os.ErrNotExist
		}
	}
}

const (
	normalTemp   = "59300\n"
	normalLoad   = "0.70 0.83 0.76 2/6165 351818\n"
	normalMem    = "MemTotal:       32765032 kB\nMemFree:         2525696 kB\nMemAvailable:    5662310 kB\nBuffers:            2208 kB\n"
	normalUptime = "3290.60 33248.12\n"
)

// TestVitalsNormal verifies that clean sysfs/procfs files produce healthy checks
// and populate structured HostStatus fields.
func TestVitalsNormal(t *testing.T) {
	src := status.New(status.Options{
		Timeout:  time.Second,
		ReadFile: fakeVitalsFiles(normalTemp, normalLoad, normalMem, normalUptime),
	})
	report := src.Status(context.Background())

	if report.Host == nil {
		t.Fatal("report.Host is nil")
	}
	if report.Host.TemperatureC == nil || *report.Host.TemperatureC != 59.3 {
		t.Errorf("TemperatureC = %v, want 59.3", report.Host.TemperatureC)
	}
	if report.Host.Load == nil || *report.Host.Load != [3]float64{0.70, 0.83, 0.76} {
		t.Errorf("Load = %v, want [0.70 0.83 0.76]", report.Host.Load)
	}
	wantMem := uint64(5662310 * 1024)
	if report.Host.MemoryAvailableBytes == nil || *report.Host.MemoryAvailableBytes != wantMem {
		t.Errorf("MemoryAvailableBytes = %v, want %d", report.Host.MemoryAvailableBytes, wantMem)
	}
	if report.Host.UptimeSeconds == nil || *report.Host.UptimeSeconds != 3290.60 {
		t.Errorf("UptimeSeconds = %v, want 3290.60", report.Host.UptimeSeconds)
	}

	checks := checksByName(report.HostChecks)
	if c := checks["CPU temperature"]; c.State != tool.StatusOK || c.Label != "ok" || c.Detail != "59.3 °C" {
		t.Errorf("CPU temperature check = %+v", c)
	}
	if c := checks["Load average"]; c.State != tool.StatusOK || c.Label != "ok" || !strings.Contains(c.Detail, "0.70, 0.83, 0.76") {
		t.Errorf("Load average check = %+v", c)
	}
	if c := checks["Memory available"]; c.State != tool.StatusOK || c.Label != "ok" || !strings.Contains(c.Detail, "available") {
		t.Errorf("Memory available check = %+v", c)
	}
	if c := checks["Uptime"]; c.State != tool.StatusOK || c.Label != "ok" {
		t.Errorf("Uptime check = %+v", c)
	}
}

// TestVitalsMissingFilePerSource proves that a missing file yields an unavailable
// check without defaulting to a deceptive zero.
func TestVitalsMissingFilePerSource(t *testing.T) {
	sources := []struct {
		name       string
		temp       string
		load       string
		mem        string
		uptime     string
		checkName  string
		fieldCheck func(*tool.HostStatus) bool
	}{
		{
			name:       "missing thermal",
			temp:       "",
			load:       normalLoad,
			mem:        normalMem,
			uptime:     normalUptime,
			checkName:  "CPU temperature",
			fieldCheck: func(h *tool.HostStatus) bool { return h.TemperatureC == nil },
		},
		{
			name:       "missing loadavg",
			temp:       normalTemp,
			load:       "",
			mem:        normalMem,
			uptime:     normalUptime,
			checkName:  "Load average",
			fieldCheck: func(h *tool.HostStatus) bool { return h.Load == nil },
		},
		{
			name:       "missing meminfo",
			temp:       normalTemp,
			load:       normalLoad,
			mem:        "",
			uptime:     normalUptime,
			checkName:  "Memory available",
			fieldCheck: func(h *tool.HostStatus) bool { return h.MemoryAvailableBytes == nil },
		},
		{
			name:       "missing uptime",
			temp:       normalTemp,
			load:       normalLoad,
			mem:        normalMem,
			uptime:     "",
			checkName:  "Uptime",
			fieldCheck: func(h *tool.HostStatus) bool { return h.UptimeSeconds == nil },
		},
	}

	for _, tc := range sources {
		t.Run(tc.name, func(t *testing.T) {
			src := status.New(status.Options{
				Timeout:  time.Second,
				ReadFile: fakeVitalsFiles(tc.temp, tc.load, tc.mem, tc.uptime),
			})
			report := src.Status(context.Background())
			if !tc.fieldCheck(report.Host) {
				t.Fatalf("expected nil field on %s for %s", tc.checkName, tc.name)
			}
			checks := checksByName(report.HostChecks)
			c := checks[tc.checkName]
			if c.State != "unavailable" || c.Label != "unavailable" {
				t.Errorf("%s check state/label = %s/%s, want unavailable", tc.checkName, c.State, c.Label)
			}
		})
	}
}

// TestVitalsShortLines verifies that truncated or empty lines yield unavailable rows.
func TestVitalsShortLines(t *testing.T) {
	cases := []struct {
		name      string
		temp      string
		load      string
		mem       string
		uptime    string
		checkName string
	}{
		{name: "empty temp", temp: "   \n", load: normalLoad, mem: normalMem, uptime: normalUptime, checkName: "CPU temperature"},
		{name: "short loadavg", temp: normalTemp, load: "0.70 0.83\n", mem: normalMem, uptime: normalUptime, checkName: "Load average"},
		{name: "short meminfo", temp: normalTemp, load: normalLoad, mem: "MemAvailable:\n", uptime: normalUptime, checkName: "Memory available"},
		{name: "empty uptime", temp: normalTemp, load: normalLoad, mem: normalMem, uptime: "  \n", checkName: "Uptime"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := status.New(status.Options{
				Timeout:  time.Second,
				ReadFile: fakeVitalsFiles(tc.temp, tc.load, tc.mem, tc.uptime),
			})
			report := src.Status(context.Background())
			checks := checksByName(report.HostChecks)
			c := checks[tc.checkName]
			if c.State != "unavailable" || c.Label != "unavailable" {
				t.Errorf("%s on short line = %+v, want unavailable", tc.checkName, c)
			}
		})
	}
}

// TestVitalsNonNumericLines verifies that corrupted text yields unavailable rows.
func TestVitalsNonNumericLines(t *testing.T) {
	cases := []struct {
		name      string
		temp      string
		load      string
		mem       string
		uptime    string
		checkName string
	}{
		{name: "non-numeric temp", temp: "hot\n", load: normalLoad, mem: normalMem, uptime: normalUptime, checkName: "CPU temperature"},
		{name: "non-numeric load", temp: normalTemp, load: "a b c\n", mem: normalMem, uptime: normalUptime, checkName: "Load average"},
		{name: "non-numeric mem", temp: normalTemp, load: normalLoad, mem: "MemAvailable: invalid kB\n", uptime: normalUptime, checkName: "Memory available"},
		{name: "non-numeric uptime", temp: normalTemp, load: normalLoad, mem: normalMem, uptime: "bad\n", checkName: "Uptime"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := status.New(status.Options{
				Timeout:  time.Second,
				ReadFile: fakeVitalsFiles(tc.temp, tc.load, tc.mem, tc.uptime),
			})
			report := src.Status(context.Background())
			checks := checksByName(report.HostChecks)
			c := checks[tc.checkName]
			if c.State != "unavailable" || c.Label != "unavailable" {
				t.Errorf("%s on non-numeric line = %+v, want unavailable", tc.checkName, c)
			}
		})
	}
}

// TestVitalsTemperatureThreshold tests temperature above and below 80 °C.
func TestVitalsTemperatureThreshold(t *testing.T) {
	// 80.0 °C is at threshold (not above).
	srcNormal := status.New(status.Options{
		Timeout:  time.Second,
		ReadFile: fakeVitalsFiles("80000\n", normalLoad, normalMem, normalUptime),
	})
	repNormal := srcNormal.Status(context.Background())
	tempCheckNormal := checksByName(repNormal.HostChecks)["CPU temperature"]
	if tempCheckNormal.State != tool.StatusOK || tempCheckNormal.Label != "ok" {
		t.Errorf("at 80.0 °C: %+v, want ok", tempCheckNormal)
	}

	// 80.1 °C is above threshold (hot).
	srcHot := status.New(status.Options{
		Timeout:  time.Second,
		ReadFile: fakeVitalsFiles("80100\n", normalLoad, normalMem, normalUptime),
	})
	repHot := srcHot.Status(context.Background())
	tempCheckHot := checksByName(repHot.HostChecks)["CPU temperature"]
	if tempCheckHot.State != tool.StatusError || tempCheckHot.Label != "hot" {
		t.Errorf("at 80.1 °C: %+v, want hot error", tempCheckHot)
	}
}

// TestVitalsMemoryThreshold tests available memory above and below 300 MB.
func TestVitalsMemoryThreshold(t *testing.T) {
	// Above 300 MB: 301 MB (308224 kB).
	mem301MB := fmt.Sprintf("MemAvailable: %d kB\n", 301*1024)
	srcNormal := status.New(status.Options{
		Timeout:  time.Second,
		ReadFile: fakeVitalsFiles(normalTemp, normalLoad, mem301MB, normalUptime),
	})
	repNormal := srcNormal.Status(context.Background())
	memCheckNormal := checksByName(repNormal.HostChecks)["Memory available"]
	if memCheckNormal.State != tool.StatusOK || memCheckNormal.Label != "ok" {
		t.Errorf("at 301 MB: %+v, want ok", memCheckNormal)
	}

	// Below 300 MB: 299 MB (306176 kB).
	mem299MB := fmt.Sprintf("MemAvailable: %d kB\n", 299*1024)
	srcLow := status.New(status.Options{
		Timeout:  time.Second,
		ReadFile: fakeVitalsFiles(normalTemp, normalLoad, mem299MB, normalUptime),
	})
	repLow := srcLow.Status(context.Background())
	memCheckLow := checksByName(repLow.HostChecks)["Memory available"]
	if memCheckLow.State != tool.StatusError || memCheckLow.Label != "low memory" {
		t.Errorf("at 299 MB: %+v, want low memory error", memCheckLow)
	}
}

// TestVitalsBothCrossed verifies that when both temperature and memory thresholds
// are crossed, both rows reflect their respective errors.
func TestVitalsBothCrossed(t *testing.T) {
	mem200MB := fmt.Sprintf("MemAvailable: %d kB\n", 200*1024)
	src := status.New(status.Options{
		Timeout:  time.Second,
		ReadFile: fakeVitalsFiles("85000\n", normalLoad, mem200MB, normalUptime),
	})
	report := src.Status(context.Background())
	checks := checksByName(report.HostChecks)

	if c := checks["CPU temperature"]; c.State != tool.StatusError || c.Label != "hot" {
		t.Errorf("CPU temperature = %+v, want hot", c)
	}
	if c := checks["Memory available"]; c.State != tool.StatusError || c.Label != "low memory" {
		t.Errorf("Memory available = %+v, want low memory", c)
	}
}

// setupTestDB creates an in-memory or file SQLite database with the activity table.
func setupTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	createTable := `
	CREATE TABLE activity (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER NOT NULL
	);
	CREATE INDEX activity_at ON activity (at DESC, id DESC);
	`
	if _, err := db.Exec(createTable); err != nil {
		t.Fatalf("create activity table: %v", err)
	}
	return db
}

// TestStorageDatabaseAndWALFiles verifies file size reporting for db and WAL.
func TestStorageDatabaseAndWALFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "assistant.db")
	walPath := dbPath + "-wal"

	db := setupTestDB(t, dbPath)
	defer db.Close()

	if err := os.WriteFile(walPath, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	src := status.New(status.Options{
		Timeout:  time.Second,
		DB:       db,
		DBPath:   dbPath,
		ReadFile: fakeVitalsFiles(normalTemp, normalLoad, normalMem, normalUptime),
	})
	report := src.Status(context.Background())

	if report.Storage == nil {
		t.Fatal("report.Storage is nil")
	}
	if report.Storage.DatabaseSizeBytes == nil || *report.Storage.DatabaseSizeBytes < 2048 {
		t.Errorf("DatabaseSizeBytes = %v, want >= 2048", report.Storage.DatabaseSizeBytes)
	}
	if report.Storage.WALSizeBytes == nil || *report.Storage.WALSizeBytes != 1024 {
		t.Errorf("WALSizeBytes = %v, want 1024", report.Storage.WALSizeBytes)
	}

	checks := checksByName(report.HostChecks)
	dbCheck := checks["Database size"]
	if dbCheck.State != tool.StatusOK || dbCheck.Label != "ok" || !strings.Contains(dbCheck.Detail, "WAL: 1 KB") {
		t.Errorf("Database size check = %+v", dbCheck)
	}
}

// TestStorageActivityRowsAndOldest verifies row count and MIN(at) index lookup.
func TestStorageActivityRowsAndOldest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "assistant.db")
	db := setupTestDB(t, dbPath)
	defer db.Close()

	// 1. Empty table.
	src := status.New(status.Options{
		Timeout:  time.Second,
		DB:       db,
		DBPath:   dbPath,
		ReadFile: fakeVitalsFiles(normalTemp, normalLoad, normalMem, normalUptime),
	})
	repEmpty := src.Status(context.Background())
	if repEmpty.Storage.ActivityRows == nil || *repEmpty.Storage.ActivityRows != 0 {
		t.Errorf("empty ActivityRows = %v, want 0", repEmpty.Storage.ActivityRows)
	}
	if repEmpty.Storage.OldestActivityAt != nil {
		t.Errorf("empty OldestActivityAt = %v, want nil", repEmpty.Storage.OldestActivityAt)
	}
	checksEmpty := checksByName(repEmpty.HostChecks)
	if checksEmpty["Activity history"].Detail != "0 rows" {
		t.Errorf("empty Activity detail = %q", checksEmpty["Activity history"].Detail)
	}

	// 2. Insert rows.
	oldestTime := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	newerTime := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	if _, err := db.Exec("INSERT INTO activity (at) VALUES (?), (?)", oldestTime.UnixMilli(), newerTime.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Wait 2 s or use a fresh status Source to bypass SharedSnapshotTTL.
	srcFilled := status.New(status.Options{
		Timeout:  time.Second,
		DB:       db,
		DBPath:   dbPath,
		ReadFile: fakeVitalsFiles(normalTemp, normalLoad, normalMem, normalUptime),
	})
	repFilled := srcFilled.Status(context.Background())
	if repFilled.Storage.ActivityRows == nil || *repFilled.Storage.ActivityRows != 2 {
		t.Errorf("ActivityRows = %v, want 2", repFilled.Storage.ActivityRows)
	}
	if repFilled.Storage.OldestActivityAt == nil || !repFilled.Storage.OldestActivityAt.Equal(oldestTime) {
		t.Errorf("OldestActivityAt = %v, want %v", repFilled.Storage.OldestActivityAt, oldestTime)
	}
	checksFilled := checksByName(repFilled.HostChecks)
	if !strings.Contains(checksFilled["Activity history"].Detail, "2 rows, oldest 2026-11-10") {
		t.Errorf("Activity detail = %q, want row count and formatted oldest date", checksFilled["Activity history"].Detail)
	}
}

// TestStorageFreeSpaceThresholds verifies low and full storage states via injected Statfs.
func TestStorageFreeSpaceThresholds(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "assistant.db")
	_ = os.WriteFile(dbPath, []byte("ok"), 0o600)

	cases := []struct {
		name        string
		blocks      uint64
		bavail      uint64
		bsize       uint64
		statErr     error
		wantState   string
		wantCheckSt tool.StatusState
		wantLabel   string
	}{
		{
			name:        "normal free space (50% and 5 GB)",
			blocks:      10 * 1024,
			bavail:      5 * 1024,
			bsize:       1024 * 1024,
			wantState:   "ok",
			wantCheckSt: tool.StatusOK,
			wantLabel:   "ok",
		},
		{
			name:        "low by percentage (8% < 10% with 800 MB)",
			blocks:      10 * 1024,
			bavail:      800,
			bsize:       1024 * 1024,
			wantState:   "low",
			wantCheckSt: tool.StatusError,
			wantLabel:   "low",
		},
		{
			name:        "low by bytes (400 MB < 500 MB even though 20%)",
			blocks:      2000,
			bavail:      400,
			bsize:       1024 * 1024,
			wantState:   "low",
			wantCheckSt: tool.StatusError,
			wantLabel:   "low",
		},
		{
			name:        "full state (1.5% < 2%)",
			blocks:      1000,
			bavail:      15,
			bsize:       1024 * 1024,
			wantState:   "full",
			wantCheckSt: tool.StatusError,
			wantLabel:   "full",
		},
		{
			name:        "statfs error yields unavailable",
			statErr:     errors.New("statfs failed"),
			wantState:   "unavailable",
			wantCheckSt: "unavailable",
			wantLabel:   "unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := status.New(status.Options{
				Timeout:  time.Second,
				DBPath:   dbPath,
				ReadFile: fakeVitalsFiles(normalTemp, normalLoad, normalMem, normalUptime),
				Statfs: func(_ string, buf *syscall.Statfs_t) error {
					if tc.statErr != nil {
						return tc.statErr
					}
					buf.Blocks = tc.blocks
					buf.Bavail = tc.bavail
					buf.Bsize = int64(tc.bsize)
					return nil
				},
			})
			rep := src.Status(context.Background())
			if rep.Storage.State != tc.wantState {
				t.Errorf("Storage.State = %q, want %q", rep.Storage.State, tc.wantState)
			}
			freeCheck := checksByName(rep.HostChecks)["Free space"]
			if freeCheck.State != tc.wantCheckSt || freeCheck.Label != tc.wantLabel {
				t.Errorf("Free space check = %+v, want state=%q label=%q", freeCheck, tc.wantCheckSt, tc.wantLabel)
			}
		})
	}
}

func checksByName(checks []tool.StatusCheck) map[string]tool.StatusCheck {
	m := make(map[string]tool.StatusCheck, len(checks))
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}
