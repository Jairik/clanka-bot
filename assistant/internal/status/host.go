package status

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const (
	// Default sysfs and procfs paths for Pi host vitals.
	ThermalZonePath = "/sys/class/thermal/thermal_zone0/temp"
	LoadAvgPath     = "/proc/loadavg"
	MemInfoPath     = "/proc/meminfo"
	UptimePath      = "/proc/uptime"

	// Thresholds for spoken warnings and health status marks.
	TempWarnThresholdC          = 80.0
	MemWarnThresholdBytes       = 300 * 1024 * 1024       // 300 MB
	StorageWarnThresholdPercent = 10.0                    // 10%
	StorageWarnThresholdBytes   = 500 * 1024 * 1024       // 500 MB
	StorageFullThresholdPercent = 2.0                     // 2%
)

// checkHostAndStorage gathers host vitals from procfs/sysfs and storage metrics
// from SQLite and Statfs in one bounded read.
func (s *Source) checkHostAndStorage(ctx context.Context) (*tool.HostStatus, *tool.StorageStatus, []tool.StatusCheck) {
	hostStatus, hostChecks := s.checkVitals()
	storageStatus, storageChecks := s.checkStorage(ctx)

	allChecks := make([]tool.StatusCheck, 0, len(hostChecks)+len(storageChecks))
	allChecks = append(allChecks, hostChecks...)
	allChecks = append(allChecks, storageChecks...)
	return hostStatus, storageStatus, allChecks
}

// checkVitals reads host-wide CPU temperature, load average, available memory,
// and uptime using the injected file reader.
func (s *Source) checkVitals() (*tool.HostStatus, []tool.StatusCheck) {
	tempVal, tempCheck := s.readTemperature()
	loadVal, loadCheck := s.readLoad()
	memVal, memCheck := s.readMemory()
	uptimeVal, uptimeCheck := s.readUptime()

	status := &tool.HostStatus{
		TemperatureC:         tempVal,
		Load:                 loadVal,
		MemoryAvailableBytes: memVal,
		UptimeSeconds:        uptimeVal,
	}
	checks := []tool.StatusCheck{tempCheck, loadCheck, memCheck, uptimeCheck}
	return status, checks
}

// readTemperature reads CPU temperature in millidegrees Celsius from sysfs.
func (s *Source) readTemperature() (*float64, tool.StatusCheck) {
	data, err := s.readFile(ThermalZonePath)
	if err != nil {
		return nil, tool.StatusCheck{
			Name:   "CPU temperature",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Thermal sensor unavailable.",
		}
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, tool.StatusCheck{
			Name:   "CPU temperature",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Thermal sensor line empty.",
		}
	}
	raw, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return nil, tool.StatusCheck{
			Name:   "CPU temperature",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Non-numeric thermal data.",
		}
	}
	tempC := float64(raw) / 1000.0
	state := tool.StatusOK
	label := "ok"
	if tempC > TempWarnThresholdC {
		state = tool.StatusError
		label = "hot"
	}
	return &tempC, tool.StatusCheck{
		Name:   "CPU temperature",
		State:  state,
		Label:  label,
		Detail: fmt.Sprintf("%.1f °C", tempC),
	}
}

// readLoad reads 1, 5, and 15-minute load averages from /proc/loadavg.
func (s *Source) readLoad() (*[3]float64, tool.StatusCheck) {
	data, err := s.readFile(LoadAvgPath)
	if err != nil {
		return nil, tool.StatusCheck{
			Name:   "Load average",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Load average unavailable.",
		}
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil, tool.StatusCheck{
			Name:   "Load average",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Load average line incomplete.",
		}
	}
	l1, err1 := strconv.ParseFloat(fields[0], 64)
	l5, err2 := strconv.ParseFloat(fields[1], 64)
	l15, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, tool.StatusCheck{
			Name:   "Load average",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Non-numeric load average.",
		}
	}
	load := [3]float64{l1, l5, l15}
	return &load, tool.StatusCheck{
		Name:   "Load average",
		State:  tool.StatusOK,
		Label:  "ok",
		Detail: fmt.Sprintf("%.2f, %.2f, %.2f (1/5/15 min)", l1, l5, l15),
	}
}

// readMemory reads MemAvailable in kB from /proc/meminfo and converts it to bytes.
func (s *Source) readMemory() (*uint64, tool.StatusCheck) {
	data, err := s.readFile(MemInfoPath)
	if err != nil {
		return nil, tool.StatusCheck{
			Name:   "Memory available",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Memory info unavailable.",
		}
	}
	var availKB uint64
	var found bool
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					availKB = val
					found = true
					break
				}
			}
			// MemAvailable line present but unparseable or short.
			return nil, tool.StatusCheck{
				Name:   "Memory available",
				State:  "unavailable",
				Label:  "unavailable",
				Detail: "Non-numeric or incomplete MemAvailable line.",
			}
		}
	}
	if !found {
		return nil, tool.StatusCheck{
			Name:   "Memory available",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "MemAvailable not found in /proc/meminfo.",
		}
	}
	bytes := availKB * 1024
	state := tool.StatusOK
	label := "ok"
	if bytes < MemWarnThresholdBytes {
		state = tool.StatusError
		label = "low memory"
	}
	return &bytes, tool.StatusCheck{
		Name:   "Memory available",
		State:  state,
		Label:  label,
		Detail: fmt.Sprintf("%s available", formatBytes(bytes)),
	}
}

// readUptime reads total system uptime in seconds from /proc/uptime.
func (s *Source) readUptime() (*float64, tool.StatusCheck) {
	data, err := s.readFile(UptimePath)
	if err != nil {
		return nil, tool.StatusCheck{
			Name:   "Uptime",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Uptime unavailable.",
		}
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return nil, tool.StatusCheck{
			Name:   "Uptime",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Uptime line empty.",
		}
	}
	sec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil, tool.StatusCheck{
			Name:   "Uptime",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Non-numeric uptime data.",
		}
	}
	return &sec, tool.StatusCheck{
		Name:   "Uptime",
		State:  tool.StatusOK,
		Label:  "ok",
		Detail: formatUptime(time.Duration(sec * float64(time.Second))),
	}
}

// checkStorage inspects SQLite file sizes, Activity row counts/oldest row, and
// free space on the data volume.
func (s *Source) checkStorage(ctx context.Context) (*tool.StorageStatus, []tool.StatusCheck) {
	storage := &tool.StorageStatus{}
	if s.dbPath == "" {
		storage.State = "unavailable"
		checks := []tool.StatusCheck{
			{Name: "Database size", State: "unavailable", Label: "unavailable", Detail: "Database path not configured."},
			{Name: "Activity history", State: "unavailable", Label: "unavailable", Detail: "Database handle not configured."},
			{Name: "Free space", State: "unavailable", Label: "unavailable", Detail: "Data directory not configured."},
		}
		return storage, checks
	}

	// Two Stat calls per snapshot: database file and its WAL file.
	var dbSize *int64
	var walSize *int64
	fi, err := os.Stat(s.dbPath)
	var dbCheck tool.StatusCheck
	if err != nil {
		dbCheck = tool.StatusCheck{
			Name:   "Database size",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Database file unavailable.",
		}
	} else {
		size := fi.Size()
		dbSize = &size
		var wSize int64
		if walFi, wErr := os.Stat(s.dbPath + "-wal"); wErr == nil {
			wSize = walFi.Size()
		}
		walSize = &wSize
		dbCheck = tool.StatusCheck{
			Name:   "Database size",
			State:  tool.StatusOK,
			Label:  "ok",
			Detail: fmt.Sprintf("%s (WAL: %s)", formatBytes(uint64(size)), formatBytes(uint64(wSize))),
		}
	}
	storage.DatabaseSizeBytes = dbSize
	storage.WALSizeBytes = walSize

	// Activity row count and oldest row via existing single writer database handle.
	var actCheck tool.StatusCheck
	if s.db == nil {
		actCheck = tool.StatusCheck{
			Name:   "Activity history",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Database handle not configured.",
		}
	} else {
		var count int
		if cErr := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM activity").Scan(&count); cErr != nil {
			actCheck = tool.StatusCheck{
				Name:   "Activity history",
				State:  "unavailable",
				Label:  "unavailable",
				Detail: "Activity table unavailable.",
			}
		} else {
			storage.ActivityRows = &count
			var oldestMillis sql.NullInt64
			// MIN(at) uses the activity_at index.
			_ = s.db.QueryRowContext(ctx, "SELECT MIN(at) FROM activity").Scan(&oldestMillis)
			if oldestMillis.Valid {
				oldest := time.UnixMilli(oldestMillis.Int64).UTC()
				storage.OldestActivityAt = &oldest
				actCheck = tool.StatusCheck{
					Name:   "Activity history",
					State:  tool.StatusOK,
					Label:  "ok",
					Detail: fmt.Sprintf("%d rows, oldest %s", count, oldest.Format("2006-01-02 15:04:05 UTC")),
				}
			} else {
				actCheck = tool.StatusCheck{
					Name:   "Activity history",
					State:  tool.StatusOK,
					Label:  "ok",
					Detail: fmt.Sprintf("%d rows", count),
				}
			}
		}
	}

	// Exactly one Statfs call on the data directory.
	var freeCheck tool.StatusCheck
	dataDir := filepath.Dir(s.dbPath)
	var stat syscall.Statfs_t
	if sfErr := s.statfs(dataDir, &stat); sfErr != nil {
		storage.State = "unavailable"
		freeCheck = tool.StatusCheck{
			Name:   "Free space",
			State:  "unavailable",
			Label:  "unavailable",
			Detail: "Data volume free space unavailable.",
		}
	} else {
		bavail := uint64(stat.Bavail)
		blocks := uint64(stat.Blocks)
		bsize := uint64(stat.Bsize)
		freeBytes := bavail * bsize
		totalBytes := blocks * bsize
		storage.FreeBytes = &freeBytes
		storage.TotalBytes = &totalBytes

		freePct := 0.0
		if totalBytes > 0 {
			freePct = float64(freeBytes) / float64(totalBytes) * 100.0
			storage.FreePercent = &freePct
		}

		switch {
		case totalBytes > 0 && freePct < StorageFullThresholdPercent:
			storage.State = "full"
			freeCheck = tool.StatusCheck{
				Name:   "Free space",
				State:  tool.StatusError,
				Label:  "full",
				Detail: fmt.Sprintf("%s free (%s)", formatBytes(freeBytes), formatPercent(freePct)),
			}
		case (totalBytes > 0 && freePct < StorageWarnThresholdPercent) || freeBytes < StorageWarnThresholdBytes:
			storage.State = "low"
			freeCheck = tool.StatusCheck{
				Name:   "Free space",
				State:  tool.StatusError,
				Label:  "low",
				Detail: fmt.Sprintf("%s free (%s)", formatBytes(freeBytes), formatPercent(freePct)),
			}
		default:
			storage.State = "ok"
			freeCheck = tool.StatusCheck{
				Name:   "Free space",
				State:  tool.StatusOK,
				Label:  "ok",
				Detail: fmt.Sprintf("%s free (%s)", formatBytes(freeBytes), formatPercent(freePct)),
			}
		}
	}

	checks := []tool.StatusCheck{dbCheck, actCheck, freeCheck}
	return storage, checks
}

// formatBytes formats byte sizes into concise binary units.
func formatBytes(b uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		val := float64(b) / float64(gib)
		if val == float64(int(val)) {
			return fmt.Sprintf("%d GB", int(val))
		}
		return fmt.Sprintf("%.1f GB", val)
	case b >= mib:
		val := float64(b) / float64(mib)
		if val == float64(int(val)) {
			return fmt.Sprintf("%d MB", int(val))
		}
		return fmt.Sprintf("%.1f MB", val)
	case b >= kib:
		val := float64(b) / float64(kib)
		if val == float64(int(val)) {
			return fmt.Sprintf("%d KB", int(val))
		}
		return fmt.Sprintf("%.1f KB", val)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatPercent formats a percentage value cleanly.
func formatPercent(p float64) string {
	if p == float64(int(p)) {
		return fmt.Sprintf("%d%%", int(p))
	}
	return fmt.Sprintf("%.1f%%", p)
}

// formatUptime turns a duration into a human-friendly uptime phrase.
func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 1:
		return fmt.Sprintf("%d days, %d hours", days, hours)
	case days == 1:
		return fmt.Sprintf("1 day, %d hours", hours)
	case hours > 0:
		return fmt.Sprintf("%d hours, %d min", hours, mins)
	default:
		return fmt.Sprintf("%d min", mins)
	}
}
