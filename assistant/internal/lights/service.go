package lights

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"os/exec"
	"sync"
	"time"
)

//go:embed ble.py
var helper string

// Device is a saved controller with an explicitly selected protocol.
type Device struct {
	Address  string `json:"address"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// Definitions keeps both selection and opt-in in the one SQLite settings store.
func Definitions() []settings.Definition {
	return []settings.Definition{settings.Bool("lights.enabled", false), settings.String("lights.devices", "[]", settings.WithValidator(func(v any) error {
		var ds []Device
		if err := json.Unmarshal([]byte(v.(string)), &ds); err != nil {
			return err
		}
		if len(ds) > 20 {
			return errors.New("at most 20 lights")
		}
		seen := map[string]bool{}
		for _, d := range ds {
			if len(d.Name) == 0 || len(d.Name) > 80 || len(d.Address) != 17 || seen[d.Address] {
				return errors.New("invalid or duplicate device")
			}
			for i, c := range d.Address {
				if i%3 == 2 {
					if c != ':' {
						return errors.New("invalid address")
					}
				} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
					return errors.New("invalid address")
				}
			}
			if d.Protocol != "lotus" && d.Protocol != "happy" {
				return errors.New("invalid protocol")
			}
			seen[d.Address] = true
		}
		return nil
	}))}
}

// Service shares serialization and last transport outcome between Tools and Dashboard.
type Service struct {
	Settings func() *settings.Service
	gate     chan struct{}
	mu       sync.Mutex
	last     string
	Run      func(context.Context, ...string) ([]byte, error)
}

func New(get func() *settings.Service) *Service {
	return &Service{Settings: get, gate: make(chan struct{}, 1)}
}
func (s *Service) Enabled() bool {
	return s != nil && s.Settings != nil && s.Settings() != nil && s.Settings().Bool("lights.enabled")
}
func (s *Service) Devices() []Device {
	ds := []Device{}
	if s.Settings != nil && s.Settings() != nil {
		_ = json.Unmarshal([]byte(s.Settings().String("lights.devices")), &ds)
	}
	return ds
}
func (s *Service) Last() string { s.mu.Lock(); defer s.mu.Unlock(); return s.last }

// writeBudget is the shared limit for queueing plus one helper run.
// A cold ELK-BLEDOM connect on the Pi took about eight seconds, so the old
// seven-second cap gave up before the controller answered. A missing
// controller still fails inside this budget.
const writeBudget = 14 * time.Second

// run bounds queueing and subprocess execution together, including missing hardware.
func (s *Service) run(ctx context.Context, args ...string) ([]byte, error) {
	if !s.Enabled() {
		return nil, errors.New("lights are disabled on the Lights page")
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.Run != nil {
		return s.Run(ctx, args...)
	}
	return exec.CommandContext(ctx, "python3", append([]string{"-c", helper}, args...)...).Output()
}
func (s *Service) Scan(ctx context.Context) ([]Device, error) {
	out, err := s.run(ctx, "scan")
	if err != nil {
		return nil, errors.New("BLE scan failed; check BlueZ D-Bus access")
	}
	var ds []Device
	err = json.Unmarshal(out, &ds)
	return ds, err
}

// Apply resolves only saved devices; no arbitrary Bluetooth target can be reached by a Tool.
func (s *Service) Apply(ctx context.Context, device, op, value string) error {
	ds := s.Devices()
	var chosen *Device
	for i := range ds {
		if ds[i].Address == device || ds[i].Name == device || device == "" && len(ds) == 1 {
			if chosen != nil {
				return errors.New("device name is ambiguous")
			}
			chosen = &ds[i]
		}
	}
	if chosen == nil {
		return errors.New("choose a saved light")
	}
	d := BLEDriver{chosen.Protocol}
	payload, err := d.Encode(op, value)
	if err != nil {
		return err
	}
	_, err = s.run(ctx, chosen.Address, d.Characteristic(), hex.EncodeToString(payload))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.last = "Last write failed; controller unavailable"
		return errors.New(s.last)
	}
	s.last = "Last write accepted; physical state unverified"
	return nil
}
