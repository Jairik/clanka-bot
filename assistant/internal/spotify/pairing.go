package spotify

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// PairingState is the Connect receiver's state read from its heartbeat.
type PairingState string

const (
	// PairingNotConfigured means no status file is wired in this deployment.
	PairingNotConfigured PairingState = "not_configured"
	// PairingUnavailable means the file is missing, malformed or stale, or the
	// receiver's control socket is not answering, so the page cannot honestly
	// claim a pairing or playback state.
	PairingUnavailable PairingState = "unavailable"
	// PairingNotPaired means the receiver is running but has no credentials.
	PairingNotPaired PairingState = "not_paired"
	// PairingPaired means the receiver has cached Connect credentials.
	PairingPaired PairingState = "paired"
	// PairingExpired means the receiver's build has passed its expiry date and
	// cannot play, even though the daemon may still answer.
	PairingExpired PairingState = "expired"
)

// PairingStatus is the safe, metadata-only view of the receiver's state. It
// never carries credentials: the writer reports only login/active booleans,
// the device name and the build's remaining days.
type PairingStatus struct {
	State      PairingState
	Label      string
	Detail     string
	Configured bool
	Available  bool
	Paired     bool
	// Session is the receiver's last reported activity: active, idle or
	// unknown. It is not proof of playback.
	Session string
	Device  string
	// ExpiresDays is the remaining lifetime of the proprietary build. It is
	// meaningful only when ExpiryKnown is true; a negative value is past the
	// expiry date.
	ExpiresDays int
	ExpiryKnown bool
	Expired     bool
	UpdatedAt   time.Time
}

// PairingReader reads the heartbeat file the Soloist entrypoint writes to its
// status directory. The file is mounted read-only into the assistant; the
// credentials and the API key are never mounted here.
type PairingReader struct {
	// Path is the heartbeat file. Empty disables the reader.
	Path string
	// StaleAfter bounds an honest heartbeat age. Zero uses DefaultStaleAfter.
	StaleAfter time.Duration
	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// Status reads and validates the heartbeat. A missing or stale file is
// unavailable, never a guessed "unpaired" or "paired".
func (r PairingReader) Status() PairingStatus {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	staleAfter := r.StaleAfter
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	status := PairingStatus{
		State:      PairingNotConfigured,
		Label:      "not configured",
		Detail:     "Soloist pairing is not configured in this deployment.",
		Configured: strings.TrimSpace(r.Path) != "",
	}
	if !status.Configured {
		return status
	}

	status.State = PairingUnavailable
	status.Label = "unavailable"
	raw, err := readSmallFile(r.Path)
	if err != nil {
		status.Detail = "Soloist's status heartbeat is unavailable."
		return status
	}
	values := make(map[string]string, 8)
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if values["version"] != "2" {
		status.Detail = "Soloist's status heartbeat has an unknown format."
		return status
	}
	updated, err := time.Parse(time.RFC3339, values["updated"])
	if err != nil {
		status.Detail = "Soloist's status heartbeat has no valid timestamp."
		return status
	}
	status.UpdatedAt = updated.UTC()
	if status.UpdatedAt.After(now().Add(time.Minute)) {
		status.Detail = "Soloist's status heartbeat timestamp is in the future."
		return status
	}
	if age := now().Sub(status.UpdatedAt); age > staleAfter {
		status.Detail = fmt.Sprintf("Soloist has not updated its status for %s.", age.Round(time.Second))
		return status
	}
	switch values["state"] {
	case "paired", "unpaired", "unavailable":
	default:
		status.Detail = "Soloist's status heartbeat has an unknown pairing state."
		return status
	}
	if values["state"] == "unavailable" {
		status.Detail = "Soloist's control socket is not answering."
		return status
	}
	if rawDays, ok := values["expires_days"]; ok && rawDays != "" && rawDays != "unknown" {
		days, err := strconv.Atoi(rawDays)
		if err != nil || days < -3650 || days > 3650 {
			status.Detail = "Soloist's status heartbeat has an unknown build expiry."
			return status
		}
		status.ExpiresDays = days
		status.ExpiryKnown = true
	}

	status.Available = true
	status.Device = values["device"]
	if status.Device == "" {
		status.Device = DefaultDeviceName
	}
	status.Session = values["session"]
	switch status.Session {
	case "active", "idle":
	default:
		status.Session = "unknown"
	}

	if status.ExpiryKnown && status.ExpiresDays < 0 {
		status.Expired = true
		status.State = PairingExpired
		status.Label = "build expired"
		status.Paired = values["state"] == "paired"
		status.Detail = fmt.Sprintf("The Soloist build expired %d day(s) ago, so playback is unavailable. Update it (docs/deploy.md).", -status.ExpiresDays)
		return status
	}

	if values["state"] == "paired" {
		status.State = PairingPaired
		status.Label = "paired"
		status.Paired = true
		status.Detail = "Paired as " + status.Device + " — cached Connect credentials are present"
	} else {
		status.State = PairingNotPaired
		status.Label = "not paired"
		status.Detail = "Not paired — choose " + status.Device + " in Spotify → Devices"
	}
	switch status.Session {
	case "active":
		status.Detail += "; the receiver is the active Spotify device."
	case "idle":
		status.Detail += "; no Spotify client is using the receiver right now."
	default:
		status.Detail += "."
	}
	if status.ExpiryKnown && status.ExpiresDays <= 14 {
		if status.ExpiresDays == 0 {
			status.Detail += " The Soloist build expires today; update it (docs/deploy.md)."
		} else {
			status.Detail += fmt.Sprintf(" The Soloist build expires in %d days; update it (docs/deploy.md).", status.ExpiresDays)
		}
	}
	return status
}

// readSmallFile bounds the heartbeat read so a corrupted or hostile file
// cannot grow the Dashboard's memory. The real file is a few short lines.
func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maxStatusBytes = 4 << 10
	raw, err := io.ReadAll(io.LimitReader(file, maxStatusBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxStatusBytes {
		return nil, fmt.Errorf("spotify: status file is too large")
	}
	return raw, nil
}
