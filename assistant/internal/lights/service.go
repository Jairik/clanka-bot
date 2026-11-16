package lights

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
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
	// Names resolves spoken device names through the one shared matcher; nil
	// keeps exact and normalised spellings working without an embed service.
	Names  *namematch.Matcher
	gate   chan struct{}
	mu     sync.Mutex
	last   string
	failed bool
	Run    func(context.Context, ...string) ([]byte, error)
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

// Failed reports whether the last transport attempt with an answer failed, so
// Health does not have to parse the detail text.
func (s *Service) Failed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.failed }

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
	return s.runWithin(ctx, args...)
}

// runWithin runs one helper invocation under the caller's existing deadline.
// A batch of lights shares one writeBudget through it, so the device count
// cannot multiply the static Tool Timeout.
func (s *Service) runWithin(ctx context.Context, args ...string) ([]byte, error) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.Run != nil {
		return s.Run(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "python3", append([]string{"-c", helper}, args...)...).Output()
	if err != nil && ctx.Err() != nil {
		// The helper was killed by the shared budget; the deadline is the reason.
		return nil, ctx.Err()
	}
	return out, err
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

// AllTarget is the spoken target for every saved light. A Tool may also omit
// the device: with one saved light that is the only choice, and with several
// it means all of them, which is what the Fast path sends only when the
// request says "lights" or "all".
const AllTarget = "all"

// Result reports one Apply attempt: how many chosen devices accepted the
// write out of how many were selected.
type Result struct {
	Done  int
	Total int
}

// Reply is the spoken line for a finished batch. A partial batch reports the
// count; Health carries the name and reason of any device that did not answer.
func (r Result) Reply() string {
	if r.Total > 1 && r.Done < r.Total {
		return fmt.Sprintf("Done for %d of %d lights", r.Done, r.Total)
	}
	return "Light command sent."
}

// Apply resolves only saved devices; no arbitrary Bluetooth target can be
// reached by a Tool. It writes to every selected device in saved order under
// one shared budget and reports how many answered.
func (s *Service) Apply(ctx context.Context, device, op, value string) (Result, error) {
	if !s.Enabled() {
		return Result{}, errors.New("lights are disabled on the Lights page")
	}
	chosen, err := s.resolve(ctx, device, s.Devices())
	if err != nil {
		return Result{}, err
	}
	// One budget for the whole batch: a cold connect is slow, and a Tool
	// Timeout stays static whether one light or all of them are written.
	batchCtx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	result := Result{Total: len(chosen)}
	var failed []string
	for i := range chosen {
		d := BLEDriver{chosen[i].Protocol}
		payload, err := d.Encode(op, value)
		if err != nil {
			return result, err
		}
		_, err = s.runWithin(batchCtx, chosen[i].Address, d.Characteristic(), hex.EncodeToString(payload))
		if err != nil && errors.Is(err, context.Canceled) {
			// The caller went away; that is not evidence about the controllers.
			return result, errors.New("light write cancelled")
		}
		if err != nil {
			failed = append(failed, chosen[i].Name+": "+errText(err))
			continue
		}
		result.Done++
	}
	s.record(result, failed)
	switch {
	case len(failed) == 0:
		return result, nil
	case result.Done > 0:
		// Partial success is not a spoken error: Reply reports the count.
		return result, nil
	case len(chosen) == 1:
		return result, errors.New("the light did not answer")
	default:
		return result, errors.New("none of the lights answered")
	}
}

// resolve chooses the saved devices a request targets. It accepts an exact
// address, the "all" target, and a saved name through the shared matcher
// (case, punctuation and speech-to-text slips normalised). It never guesses:
// an ambiguous name is an error, and an unmatched name is not a light.
func (s *Service) resolve(ctx context.Context, device string, ds []Device) ([]Device, error) {
	device = strings.TrimSpace(device)
	if len(ds) == 0 {
		return nil, errors.New("choose a saved light")
	}
	if device == "" || strings.EqualFold(device, AllTarget) {
		return ds, nil
	}
	for _, d := range ds {
		if d.Address == device {
			return []Device{d}, nil
		}
	}
	names := make([]string, 0, len(ds))
	for _, d := range ds {
		names = append(names, d.Name)
	}
	name, err := s.matchName(ctx, device, names)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("choose a saved light")
	}
	var chosen []Device
	for _, d := range ds {
		if d.Name == name {
			chosen = append(chosen, d)
		}
	}
	if len(chosen) != 1 {
		// Two saved controllers share the spoken name; speech cannot pick one.
		return nil, errors.New("device name is ambiguous")
	}
	return chosen, nil
}

// matchName resolves a cleaned device name against the saved names. The
// shared matcher handles case, punctuation and near-homophones; a nil matcher
// still resolves exact and normalised spellings so the Dashboard and unit
// tests work without an embed service.
func (s *Service) matchName(ctx context.Context, spoken string, names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	if s.Names != nil {
		result := s.Names.Match(ctx, namematch.SourceLights, spoken, names)
		if result.Matched() {
			return result.Name, nil
		}
		if result.Reason == namematch.ReasonAmbiguous {
			return "", errors.New("device name is ambiguous")
		}
		return "", nil
	}
	want := namematch.Normalize(spoken)
	var exact []string
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(spoken)) ||
			(want != "" && namematch.Normalize(name) == want) {
			exact = append(exact, name)
		}
	}
	switch len(exact) {
	case 0:
		return "", nil
	case 1:
		return exact[0], nil
	default:
		return "", errors.New("device name is ambiguous")
	}
}

// SpokenName resolves the saved light an utterance names, for the Fast path's
// argument filler. A name that appears in the words wins over any fuzzy
// reading, so "desk lamp" in a longer sentence needs no embedding call. With
// fuzzy set, a name mangled by speech-to-text may still resolve through the
// shared matcher; ambiguous reports that the utterance cannot pick one, which
// the caller must not turn into an action.
func (s *Service) SpokenName(ctx context.Context, text string, fuzzy bool) (name string, ambiguous bool) {
	ds := s.Devices()
	if len(ds) == 0 {
		return "", false
	}
	names := make([]string, 0, len(ds))
	for _, d := range ds {
		names = append(names, d.Name)
	}
	spoken := namematch.Normalize(text)
	best, bestLen := "", 0
	ambiguous = false
	for _, candidate := range names {
		norm := namematch.Normalize(candidate)
		if norm == "" || !containsPhrase(spoken, norm) {
			continue
		}
		switch {
		case len(norm) > bestLen:
			// The longest saved name wins: "Desk Lamp" beats a sibling "Desk".
			best, bestLen, ambiguous = candidate, len(norm), false
		case len(norm) == bestLen && candidate != best:
			ambiguous = true
		}
	}
	if best != "" || ambiguous || !fuzzy || s.Names == nil {
		return best, ambiguous
	}
	result := s.Names.Match(ctx, namematch.SourceLights, text, names)
	if result.Matched() {
		return result.Name, false
	}
	return "", result.Reason == namematch.ReasonAmbiguous
}

// containsPhrase reports whether the normalised words of phrase appear as one
// contiguous run in the normalised words of text.
func containsPhrase(text, phrase string) bool {
	return strings.Contains(" "+text+" ", " "+phrase+" ")
}

// record keeps the last transport outcome for Health. A batch names every
// device that failed, with the helper's reason, so the Lights page points at
// the controller instead of the whole feature.
func (s *Service) record(result Result, failed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(failed) == 0 {
		s.failed = false
		s.last = "Last write accepted; physical state unverified"
		return
	}
	s.failed = true
	detail := strings.Join(failed, "; ")
	if result.Total > 1 {
		detail = fmt.Sprintf("%d of %d lights answered; %s", result.Done, result.Total, detail)
	}
	s.last = trimDetail(detail, 300)
}

// errText keeps one Health-sized line from a failed helper run. The helper's
// traceback is the only place the transport says why, so a generic
// "controller unavailable" would hide an unreachable or incompatible device.
func errText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer within " + writeBudget.String()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		lines := strings.Split(strings.TrimSpace(string(exit.Stderr)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			// "bleak.exc.BleakError: failed to discover services" keeps the reason.
			if k := strings.Index(line, ": "); k > 0 && strings.HasSuffix(line[:k], "Error") {
				line = line[k+2:]
			}
			return trimDetail(line, 140)
		}
	}
	return trimDetail(err.Error(), 140)
}

func trimDetail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}
