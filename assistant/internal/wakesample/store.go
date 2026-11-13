package wakesample

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// MinTakes is the smallest finished positive set the desktop training CLI
	// will import.
	MinTakes = 5
	// MaxTakes caps each kind per phrase. The issue's limit is 100 takes per
	// phrase; applying it per kind keeps a negative set from growing without
	// bound too.
	MaxTakes = 100
	// MaxTakeBytes bounds one upload body. Three seconds of 16 kHz mono
	// 16-bit is ~96 KB, so this is generous without being a memory risk.
	MaxTakeBytes = 512 << 10
	// MaxBundleBytes bounds one submitted bundle's download.
	MaxBundleBytes = 32 << 20

	bundleFormat = 1
)

// Kind is the two sample classes the #77 recorder writes.
type Kind string

const (
	Positive Kind = "positive"
	Negative Kind = "negative"
)

// Valid reports a known kind.
func (k Kind) Valid() bool { return k == Positive || k == Negative }

func (k Kind) dir() string {
	if k == Negative {
		return "negative"
	}
	return "positive"
}

func (k Kind) prefix() string {
	if k == Negative {
		return "real_n_"
	}
	return "real_p_"
}

// ParseKind accepts the path segment the Dashboard sends.
func ParseKind(value string) (Kind, bool) {
	kind := Kind(value)
	return kind, kind.Valid()
}

// Take is one stored clip.
type Take struct {
	ID       string
	Kind     Kind
	Duration time.Duration
	Bytes    int64
	AddedAt  time.Time
}

// Bundle is one submitted sample set in the host-visible export directory.
type Bundle struct {
	Name      string
	Slug      string
	Phrase    string
	Positives int
	Negatives int
	Bytes     int64
	CreatedAt time.Time
	// HostPath is where the owner's desktop can reach the file when the
	// deployment configured the bind mount's host path; empty otherwise.
	HostPath string
	// SCP and Taildrop are ready-to-run copy commands, empty when the
	// deployment has not configured the user/host/desktop.
	SCP      string
	Taildrop string
}

// Options configures the store. Paths are resolved by the caller; a relative
// path here is used as given.
type Options struct {
	// Dir holds the takes, one subdirectory per phrase.
	Dir string
	// ExportDir holds finished .tar.gz bundles. It should be the host-visible
	// bind mount so the copy command names a path the owner can scp.
	ExportDir string
	// HostDir is that same directory as the host sees it. Empty hides the
	// copy commands, because a container path would be wrong on the desktop.
	HostDir string
	// SSHUser and SSHHost build the scp command (`rbpi@pi`). Both empty hides
	// it.
	SSHUser string
	SSHHost string
	// Desktop is the Taildrop target (`tailscale file cp <bundle> <desktop>:`).
	// Empty hides that form.
	Desktop string
	// MaxTakes caps each kind; zero uses the package default.
	MaxTakes int
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Service is the take store and bundle exporter. It has no database: a sample
// set is files the owner explicitly copies off the box, never history.
type Service struct {
	dir       string
	exportDir string
	hostDir   string
	sshUser   string
	sshHost   string
	desktop   string
	maxTakes  int
	now       func() time.Time

	// mu serializes the writers. Add picks the next take number from a
	// directory listing, so two concurrent keeps would otherwise choose the
	// same id and the second rename would silently replace the first.
	mu sync.Mutex
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+){0,3}$`)
var takePattern = regexp.MustCompile(`^real_[pn]_(\d{5})$`)
var bundlePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+){0,3}-\d{8}-\d{6}\.tar\.gz$`)

// New builds the store, creating both directories.
func New(opts Options) (*Service, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return nil, errors.New("wakesample: dir is required")
	}
	exportDir := strings.TrimSpace(opts.ExportDir)
	if exportDir == "" {
		exportDir = filepath.Join(dir, "exports")
	}
	for _, path := range []string{dir, exportDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("wakesample: create %s: %w", path, err)
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxTakes := opts.MaxTakes
	if maxTakes < MinTakes || maxTakes > MaxTakes {
		maxTakes = MaxTakes
	}
	return &Service{
		dir:       dir,
		exportDir: exportDir,
		hostDir:   strings.TrimSpace(opts.HostDir),
		sshUser:   strings.TrimSpace(opts.SSHUser),
		sshHost:   strings.TrimSpace(opts.SSHHost),
		desktop:   strings.TrimSpace(opts.Desktop),
		maxTakes:  maxTakes,
		now:       now,
	}, nil
}

// Dir is the take store root.
func (s *Service) Dir() string { return s.dir }

// ExportDir is the bundle directory.
func (s *Service) ExportDir() string { return s.exportDir }

// TakeLimit is the per-kind cap this deployment configured.
func (s *Service) TakeLimit() int { return s.maxTakes }

// BuiltinPhrases are the phrases the issue names. A stored phrase directory is
// also selectable: AddPhrase creates one with no takes, so a new phrase
// survives a page reload before its first take.
var BuiltinPhrases = []string{"clanker", "volume up", "volume down"}

// PhraseText turns a slug into the phrase the bundle records: hyphen-separated
// words. The follower keeps the phrase text and slug in step without a second
// name the owner has to type.
func PhraseText(slug string) string { return strings.ReplaceAll(slug, "-", " ") }

// SlugFor normalises owner-entered words into a slug. It is deliberately
// strict: the slug is a directory name and the bundle prefix, so anything
// outside [a-z0-9-] is dropped rather than escaped.
func SlugFor(phrase string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(phrase))
	if strings.ContainsAny(lower, `/\<>:"|?*.`) {
		return "", false
	}
	var b strings.Builder
	lastDash := true
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == ' ' || r == '-' || r == '_' || r == '\'':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if !ValidSlug(slug) {
		return "", false
	}
	return slug, true
}

// ValidSlug reports a path-safe phrase name. The bounds also keep a bundle
// name readable and finite.
func ValidSlug(slug string) bool {
	if len(slug) < 1 || len(slug) > 40 || !slugPattern.MatchString(slug) {
		return false
	}
	// A phrase is words, not only digits.
	return strings.ContainsAny(slug, "abcdefghijklmnopqrstuvwxyz")
}

// Phrases lists the built-ins followed by every phrase with stored takes.
func (s *Service) Phrases() []string {
	seen := map[string]bool{}
	phrases := make([]string, 0, len(BuiltinPhrases)+4)
	for _, phrase := range BuiltinPhrases {
		slug, ok := SlugFor(phrase)
		if !ok || seen[slug] {
			continue
		}
		seen[slug] = true
		phrases = append(phrases, slug)
	}
	entries, err := os.ReadDir(s.dir)
	if err == nil {
		var extra []string
		for _, entry := range entries {
			if entry.IsDir() && ValidSlug(entry.Name()) && !seen[entry.Name()] && !s.isExportDir(entry.Name()) {
				seen[entry.Name()] = true
				extra = append(extra, entry.Name())
			}
		}
		sort.Strings(extra)
		phrases = append(phrases, extra...)
	}
	return phrases
}

func (s *Service) kindDir(slug string, kind Kind) (string, error) {
	if !ValidSlug(slug) {
		return "", fmt.Errorf("invalid phrase %q", slug)
	}
	if !kind.Valid() {
		return "", fmt.Errorf("invalid sample kind %q", kind)
	}
	return filepath.Join(s.dir, slug, "real", kind.dir()), nil
}

// Takes lists one kind, oldest numbering first.
func (s *Service) Takes(slug string, kind Kind) ([]Take, error) {
	dir, err := s.kindDir(slug, kind)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	takes := make([]Take, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wav") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".wav")
		if !takePattern.MatchString(id) || !strings.HasPrefix(id, kind.prefix()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		duration, err := takeDuration(path)
		if err != nil {
			continue
		}
		takes = append(takes, Take{ID: id, Kind: kind, Duration: duration, Bytes: info.Size(), AddedAt: info.ModTime()})
	}
	sort.Slice(takes, func(i, j int) bool { return takes[i].ID < takes[j].ID })
	return takes, nil
}

func takeDuration(path string) (time.Duration, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	head := make([]byte, 4096)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, err
	}
	samples, err := Info(head[:n])
	if err != nil {
		return 0, err
	}
	return time.Duration(float64(samples) / SampleRate * float64(time.Second)), nil
}

// Count returns the number of takes of one kind.
func (s *Service) Count(slug string, kind Kind) int {
	takes, err := s.Takes(slug, kind)
	if err != nil {
		return 0
	}
	return len(takes)
}

// Add validates and stores one take. It refuses an over-limit set and an exact
// duplicate of a take already kept, which is what a repeated tap looks like.
func (s *Service) Add(slug string, kind Kind, data []byte) (Take, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.kindDir(slug, kind)
	if err != nil {
		return Take{}, err
	}
	if len(data) == 0 {
		return Take{}, errors.New("the take is empty")
	}
	if len(data) > MaxTakeBytes {
		return Take{}, fmt.Errorf("the take is larger than %d KB", MaxTakeBytes/1024)
	}
	audio, err := Prepare(data)
	if err != nil {
		return Take{}, err
	}
	existing, err := s.Takes(slug, kind)
	if err != nil {
		return Take{}, err
	}
	if len(existing) >= s.maxTakes {
		return Take{}, fmt.Errorf("%s already has %d takes; delete one before recording another", PhraseText(slug), s.maxTakes)
	}
	digest := digestSamples(audio.Samples)
	highest := 0
	for _, take := range existing {
		match := takePattern.FindStringSubmatch(take.ID)
		if number, err := strconv.Atoi(match[1]); err == nil && number > highest {
			highest = number
		}
		other, err := readTakeSamples(filepath.Join(dir, take.ID+".wav"))
		if err == nil && digestSamples(other) == digest {
			return Take{}, errors.New("that take is identical to one already kept; record a fresh one")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Take{}, err
	}
	id := fmt.Sprintf("%s%05d", kind.prefix(), highest+1)
	path := filepath.Join(dir, id+".wav")
	tmp, err := os.CreateTemp(dir, ".take-*.wav")
	if err != nil {
		return Take{}, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(EncodeWAV(audio.Samples)); err != nil {
		tmp.Close()
		cleanup()
		return Take{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return Take{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return Take{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return Take{}, err
	}
	info, _ := os.Stat(path)
	take := Take{ID: id, Kind: kind, Duration: time.Duration(audio.Duration() * float64(time.Second))}
	if info != nil {
		take.Bytes = info.Size()
		take.AddedAt = info.ModTime()
	}
	return take, nil
}

func readTakeSamples(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseWAV(data)
}

func digestSamples(samples []int16) string {
	hash := sha256.New()
	var pair [2]byte
	for _, sample := range samples {
		pair[0] = byte(sample)
		pair[1] = byte(sample >> 8)
		_, _ = hash.Write(pair[:])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// Delete removes one take. A missing id is not an error, so a double tap is
// harmless.
func (s *Service) Delete(slug string, kind Kind, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.kindDir(slug, kind)
	if err != nil {
		return err
	}
	if !takePattern.MatchString(id) || !strings.HasPrefix(id, kind.prefix()) {
		return errors.New("invalid take id")
	}
	err = os.Remove(filepath.Join(dir, id+".wav"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Clear removes every take of one kind.
func (s *Service) Clear(slug string, kind Kind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.kindDir(slug, kind)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wav") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Path is one take's canonical WAV path.
func (s *Service) Path(slug string, kind Kind, id string) (string, error) {
	dir, err := s.kindDir(slug, kind)
	if err != nil {
		return "", err
	}
	if !takePattern.MatchString(id) || !strings.HasPrefix(id, kind.prefix()) {
		return "", errors.New("invalid take id")
	}
	path := filepath.Join(dir, id+".wav")
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

// Submit writes the current positive (and optional negative) takes as one
// .tar.gz in the same layout the desktop recorder writes. The set must be a
// finished one: 5 to 100 positives.
func (s *Service) Submit(slug string) (Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidSlug(slug) {
		return Bundle{}, errors.New("invalid phrase")
	}
	positives, err := s.Takes(slug, Positive)
	if err != nil {
		return Bundle{}, err
	}
	negatives, err := s.Takes(slug, Negative)
	if err != nil {
		return Bundle{}, err
	}
	if len(positives) < MinTakes {
		return Bundle{}, fmt.Errorf("keep at least %d positive takes before submitting", MinTakes)
	}
	if len(positives) > s.maxTakes || len(negatives) > s.maxTakes {
		return Bundle{}, fmt.Errorf("a bundle holds at most %d takes of each kind", s.maxTakes)
	}
	now := s.now().UTC()
	name := ""
	var path string
	for attempt := 0; attempt < 30; attempt++ {
		candidate := fmt.Sprintf("%s-%s.tar.gz", slug, now.Add(time.Duration(attempt)*time.Second).Format("20060102-150405"))
		if _, err := os.Stat(filepath.Join(s.exportDir, candidate)); errors.Is(err, os.ErrNotExist) {
			name = candidate
			path = filepath.Join(s.exportDir, name)
			break
		}
	}
	if name == "" {
		return Bundle{}, errors.New("could not choose a bundle name")
	}
	tmp, err := os.CreateTemp(s.exportDir, ".bundle-*.tar.gz")
	if err != nil {
		return Bundle{}, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if err := s.writeBundle(tmp, slug, positives, negatives, now); err != nil {
		tmp.Close()
		cleanup()
		return Bundle{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return Bundle{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return Bundle{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return Bundle{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Bundle{}, err
	}
	bundle := Bundle{
		Name:      name,
		Slug:      slug,
		Phrase:    PhraseText(slug),
		Positives: len(positives),
		Negatives: len(negatives),
		Bytes:     info.Size(),
		CreatedAt: now,
	}
	s.decorate(&bundle)
	return bundle, nil
}

type manifest struct {
	Format      int     `json:"format"`
	Phrase      string  `json:"phrase"`
	Slug        string  `json:"slug"`
	Source      string  `json:"source"`
	CreatedAt   string  `json:"created_at"`
	Positives   int     `json:"positives"`
	Negatives   int     `json:"negatives"`
	SampleRate  int     `json:"sample_rate"`
	Channels    int     `json:"channels"`
	SampleWidth int     `json:"sample_width"`
	ClipSeconds float64 `json:"clip_seconds"`
}

func (s *Service) writeBundle(out io.Writer, slug string, positives, negatives []Take, now time.Time) error {
	gz := gzip.NewWriter(out)
	tarWriter := tar.NewWriter(gz)
	write := func(name string, data []byte) error {
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: now, Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		_, err := tarWriter.Write(data)
		return err
	}
	meta := manifest{
		Format: bundleFormat, Phrase: PhraseText(slug), Slug: slug, Source: "dashboard",
		CreatedAt: now.Format(time.RFC3339), Positives: len(positives), Negatives: len(negatives),
		SampleRate: SampleRate, Channels: Channels, SampleWidth: bytesPerSample,
		ClipSeconds: MaxClipSeconds,
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := write("manifest.json", body); err != nil {
		return err
	}
	for _, group := range []struct {
		kind  Kind
		takes []Take
	}{{Positive, positives}, {Negative, negatives}} {
		dir, err := s.kindDir(slug, group.kind)
		if err != nil {
			return err
		}
		for _, take := range group.takes {
			data, err := os.ReadFile(filepath.Join(dir, take.ID+".wav"))
			if err != nil {
				return err
			}
			if err := write("real/"+group.kind.dir()+"/"+take.ID+".wav", data); err != nil {
				return err
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Bundles lists the newest bundles for one phrase.
func (s *Service) Bundles(slug string, limit int) ([]Bundle, error) {
	if !ValidSlug(slug) {
		return nil, errors.New("invalid phrase")
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	entries, err := os.ReadDir(s.exportDir)
	if err != nil {
		return nil, err
	}
	bundles := make([]Bundle, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !bundlePattern.MatchString(name) || bundleSlug(name) != slug {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		created, err := bundleTime(name)
		if err != nil {
			continue
		}
		bundle := Bundle{Name: name, Slug: slug, Phrase: PhraseText(slug), Bytes: info.Size(), CreatedAt: created}
		if counts, ok := readBundleManifest(filepath.Join(s.exportDir, name)); ok {
			bundle.Positives = counts.Positives
			bundle.Negatives = counts.Negatives
		}
		s.decorate(&bundle)
		bundles = append(bundles, bundle)
	}
	sort.Slice(bundles, func(i, j int) bool {
		if bundles[i].CreatedAt.Equal(bundles[j].CreatedAt) {
			return bundles[i].Name > bundles[j].Name
		}
		return bundles[i].CreatedAt.After(bundles[j].CreatedAt)
	})
	if len(bundles) > limit {
		bundles = bundles[:limit]
	}
	return bundles, nil
}

func bundleTime(name string) (time.Time, error) {
	trimmed := strings.TrimSuffix(name, ".tar.gz")
	parts := strings.Split(trimmed, "-")
	stamp := ""
	if len(parts) >= 2 {
		stamp = parts[len(parts)-2] + parts[len(parts)-1]
	}
	if len(stamp) != 14 {
		return time.Time{}, errors.New("no timestamp")
	}
	return time.ParseInLocation("20060102150405", stamp, time.UTC)
}

func readBundleManifest(path string) (manifest, bool) {
	file, err := os.Open(path)
	if err != nil {
		return manifest{}, false
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return manifest{}, false
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err != nil {
			return manifest{}, false
		}
		if header.Name != "manifest.json" || header.Size > 1<<20 {
			continue
		}
		body := make([]byte, header.Size)
		if _, err := io.ReadFull(reader, body); err != nil {
			return manifest{}, false
		}
		var meta manifest
		if err := json.Unmarshal(body, &meta); err != nil {
			return manifest{}, false
		}
		return meta, true
	}
}

// OpenBundle serves one download. The name is re-checked here so a crafted
// path can never escape the export directory.
func (s *Service) OpenBundle(slug, name string) (*os.File, Bundle, error) {
	if !ValidSlug(slug) || !ValidBundleName(name) || bundleSlug(name) != slug {
		return nil, Bundle{}, errors.New("invalid bundle")
	}
	path := filepath.Join(s.exportDir, name)
	rel, err := filepath.Rel(s.exportDir, path)
	if err != nil || rel != name {
		return nil, Bundle{}, errors.New("invalid bundle")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, Bundle{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, Bundle{}, err
	}
	created, err := bundleTime(name)
	if err != nil {
		file.Close()
		return nil, Bundle{}, err
	}
	bundle := Bundle{Name: name, Slug: slug, Phrase: PhraseText(slug), Bytes: info.Size(), CreatedAt: created}
	if counts, ok := readBundleManifest(path); ok {
		bundle.Positives = counts.Positives
		bundle.Negatives = counts.Negatives
	}
	s.decorate(&bundle)
	return file, bundle, nil
}

// bundleSlug is the phrase part of a bundle name. The last two hyphen-separated
// parts are the date and time, so a multi-word slug is unambiguous.
func bundleSlug(name string) string {
	base := strings.TrimSuffix(name, ".tar.gz")
	parts := strings.Split(base, "-")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[:len(parts)-2], "-")
}

// ValidBundleName reports the exact shape Submit writes.
func ValidBundleName(name string) bool { return bundlePattern.MatchString(name) }

func (s *Service) decorate(bundle *Bundle) {
	if s.hostDir != "" {
		bundle.HostPath = filepath.Join(s.hostDir, bundle.Name)
	}
	if bundle.HostPath != "" && s.sshUser != "" && s.sshHost != "" {
		bundle.SCP = fmt.Sprintf("scp %s@%s:%s .", s.sshUser, s.sshHost, shellPath(bundle.HostPath))
	}
	if bundle.HostPath != "" && s.desktop != "" {
		bundle.Taildrop = fmt.Sprintf("tailscale file cp %s %s:", shellPath(bundle.HostPath), s.desktop)
	}
}

// shellPath quotes a local path for a one-line copy command. The bundle name
// is already restricted to [a-z0-9-] and the configured directories are the
// owner's; quoting still keeps a path with a space usable.
func shellPath(path string) string {
	if strings.ContainsAny(path, " \t'\"$`\\") {
		return strconv.Quote(path)
	}
	return path
}

// DownloadURL is the authenticated route for one bundle.
func DownloadURL(slug, name string) string {
	return "/voice-samples/bundle/" + url.PathEscape(slug) + "/" + url.PathEscape(name)
}

// AudioURL is the authenticated route for one take.
func AudioURL(slug string, kind Kind, id string) string {
	return "/voice-samples/take/" + url.PathEscape(slug) + "/" + url.PathEscape(string(kind)) + "/" + url.PathEscape(id)
}
