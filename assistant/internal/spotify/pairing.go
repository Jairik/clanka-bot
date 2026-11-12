package spotify

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// PairingState is the Connect pairing state read from librespot's heartbeat.
type PairingState string

const (
	// PairingNotConfigured means no status file is wired in this deployment.
	PairingNotConfigured PairingState = "not_configured"
	// PairingUnavailable means the file is missing, malformed or stale, so the
	// page cannot honestly claim a pairing state.
	PairingUnavailable PairingState = "unavailable"
	// PairingNotPaired means librespot is running but no credentials are cached.
	PairingNotPaired PairingState = "not_paired"
	// PairingPaired means librespot has cached Connect credentials.
	PairingPaired PairingState = "paired"
)

// PairingStatus is the safe, metadata-only view of librespot's state. It
// never carries credentials: the writer only reports whether the cache file
// exists, never its contents.
type PairingStatus struct {
	State      PairingState
	Label      string
	Detail     string
	Configured bool
	Available  bool
	Paired     bool
	// Session is librespot's last session event: connected, disconnected or
	// unknown. It is not proof of playback.
	Session   string
	Device    string
	UpdatedAt time.Time
}

// PairingReader reads the heartbeat file the librespot entrypoint writes to
// its own status volume. The file is mounted read-only into the assistant;
// the credentials cache itself is never mounted here.
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
		Detail:     "librespot pairing is not configured in this deployment.",
		Configured: strings.TrimSpace(r.Path) != "",
	}
	if !status.Configured {
		return status
	}

	status.State = PairingUnavailable
	status.Label = "unavailable"
	raw, err := readSmallFile(r.Path)
	if err != nil {
		status.Detail = "librespot's status heartbeat is unavailable."
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
	if values["version"] != "1" {
		status.Detail = "librespot's status heartbeat has an unknown format."
		return status
	}
	updated, err := time.Parse(time.RFC3339, values["updated"])
	if err != nil {
		status.Detail = "librespot's status heartbeat has no valid timestamp."
		return status
	}
	status.UpdatedAt = updated.UTC()
	if status.UpdatedAt.After(now().Add(time.Minute)) {
		status.Detail = "librespot's status heartbeat timestamp is in the future."
		return status
	}
	if age := now().Sub(status.UpdatedAt); age > staleAfter {
		status.Detail = fmt.Sprintf("librespot has not updated its status for %s.", age.Round(time.Second))
		return status
	}
	switch values["state"] {
	case "paired", "unpaired":
	default:
		status.Detail = "librespot's status heartbeat has an unknown pairing state."
		return status
	}

	status.Available = true
	status.Device = values["device"]
	if status.Device == "" {
		status.Device = DefaultDeviceName
	}
	status.Session = values["session"]
	switch status.Session {
	case "connected", "disconnected":
	default:
		status.Session = "unknown"
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
	case "connected":
		status.Detail += "; a Spotify client is connected."
	case "disconnected":
		status.Detail += "; the last Spotify client disconnected."
	default:
		status.Detail += "."
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
