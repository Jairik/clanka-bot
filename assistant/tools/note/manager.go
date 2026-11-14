package note

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/vector"
)

// storeTimeout bounds each small note query. The database is local, so a slow
// query means it is wedged, not busy.
const storeTimeout = 5 * time.Second

// embedTimeout bounds the one embed call at note write or recall time. It is
// shorter than the Tool timeout so a dead embed service returns a spoken error
// instead of cutting the whole request off.
const embedTimeout = 1500 * time.Millisecond

// ErrUnavailable reports a note operation without a database.
var ErrUnavailable = errors.New("note: the note store is not available")

// Note is one remembered fact. The cached embedding is deliberately not
// exported: it is storage detail, never part of a Tool or Dashboard result.
type Note struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	embedding []float32
}

// Match is one recall result. Score is 1 for an exact substring hit and the
// cosine similarity otherwise.
type Match struct {
	Note
	Score float64 `json:"score"`
}

// Error is a spoken validation failure from the manager.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

// Options are the Manager's seams. The zero value is usable: the clock is the
// real one, the logger is the default and a nil Embed disables semantic recall.
type Options struct {
	DB     *store.Store
	Embed  tool.Embedder
	Now    func() time.Time
	Logger *slog.Logger
	Config Config
}

// Manager owns the persisted Notes.
type Manager struct {
	db     *sql.DB
	embed  tool.Embedder
	now    func() time.Time
	logger *slog.Logger
	cfg    Config
}

// NewManager builds a Manager. A nil store is allowed so the conformance suite
// can inspect registration; every handler then fails with ErrUnavailable.
func NewManager(opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	cfg := opts.Config
	if cfg == (Config{}) {
		cfg = DefaultConfig()
	}
	var db *sql.DB
	if opts.DB != nil {
		db = opts.DB.DB()
	}
	return &Manager{db: db, embed: opts.Embed, now: opts.Now, logger: opts.Logger, cfg: cfg}
}

// Remember stores one note. Text is trimmed and bounded before any write.
func (m *Manager) Remember(ctx context.Context, text string) (Note, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, &Error{Message: "What should I remember?"}
	}
	if len([]rune(text)) > m.cfg.MaxText {
		return Note{}, &Error{Message: "That note is too long."}
	}
	if m.db == nil {
		return Note{}, ErrUnavailable
	}
	now := m.now()
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`INSERT INTO notes (text, embedding, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		text, encodeVector(m.embedText(text)), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Note{}, fmt.Errorf("note: save note: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Note{}, fmt.Errorf("note: saved note id: %w", err)
	}
	m.logger.Info("note saved", "note_id", id)
	return Note{ID: strconv.FormatInt(id, 10), Text: text, CreatedAt: now, UpdatedAt: now}, nil
}

// List returns the newest notes first, bounded by max_notes. It is the
// Dashboard read seam and the candidate set for recall.
func (m *Manager) List(ctx context.Context) ([]Note, error) {
	if m.db == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, text, embedding, created_at, updated_at FROM notes ORDER BY updated_at DESC, id DESC LIMIT ?`,
		m.cfg.MaxNotes)
	if err != nil {
		return nil, fmt.Errorf("note: list notes: %w", err)
	}
	defer rows.Close()
	var notes []Note
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("note: list notes: %w", err)
	}
	return notes, nil
}

// Update replaces one note's text.
func (m *Manager) Update(ctx context.Context, id, text string) (Note, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, &Error{Message: "A note can't be empty."}
	}
	if len([]rune(text)) > m.cfg.MaxText {
		return Note{}, &Error{Message: "That note is too long."}
	}
	noteID, err := parseID(id)
	if err != nil {
		return Note{}, err
	}
	if m.db == nil {
		return Note{}, ErrUnavailable
	}
	now := m.now()
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`UPDATE notes SET text = ?, embedding = ?, updated_at = ? WHERE id = ?`,
		text, encodeVector(m.embedText(text)), now.UnixMilli(), noteID)
	if err != nil {
		return Note{}, fmt.Errorf("note: update note: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Note{}, &Error{Message: "That note is already gone."}
	}
	m.logger.Info("note updated", "note_id", noteID)
	return Note{ID: id, Text: text, UpdatedAt: now}, nil
}

// Delete removes one note by id and returns how many rows it deleted.
func (m *Manager) Delete(ctx context.Context, id string) (int, error) {
	noteID, err := parseID(id)
	if err != nil {
		return 0, err
	}
	if m.db == nil {
		return 0, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, noteID)
	if err != nil {
		return 0, fmt.Errorf("note: delete note: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		m.logger.Info("note deleted", "note_id", noteID)
	}
	return int(affected), nil
}

// ForgetQuery deletes the closest note to query. It deletes one note, never a
// batch: an ambiguous phrase must not wipe unrelated facts.
func (m *Manager) ForgetQuery(ctx context.Context, query string) (int, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, &Error{Message: "Which note should I forget?"}
	}
	matches, err := m.Recall(ctx, query)
	if err != nil {
		return 0, err
	}
	if len(matches) == 0 {
		return 0, nil
	}
	return m.Delete(ctx, matches[0].ID)
}

// Recall ranks the notes against a query by meaning. An exact substring hit
// wins before any embedding call, so a dead embed service still answers the
// explicit case. No match is a real answer, not an error.
func (m *Manager) Recall(ctx context.Context, query string) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	notes, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(notes) == 0 {
		return nil, nil
	}

	lowerQuery := strings.ToLower(query)
	var exact []Match
	for _, note := range notes {
		if strings.Contains(strings.ToLower(note.Text), lowerQuery) {
			exact = append(exact, Match{Note: note, Score: 1})
		}
	}
	if len(exact) > 0 {
		return limitMatches(exact, m.cfg.MaxResults), nil
	}
	if m.embed == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Only the query is embedded per recall: each note's vector was captured
	// when it was written or edited, so a large note set cannot blow the Tool
	// timeout or the embed service budget.
	vectors, err := m.embed.Embed(ctx, []string{query})
	if err != nil {
		m.logger.Warn("note recall embed failed", "error", err.Error())
		return nil, err
	}
	if err := validateVectors(vectors, 1); err != nil {
		m.logger.Warn("note recall embed response invalid", "error", err.Error())
		return nil, err
	}

	queryVector := vectors[0]
	matches := make([]Match, 0, len(notes))
	for _, note := range notes {
		if len(note.embedding) == 0 || len(note.embedding) != len(queryVector) {
			// A note saved while the embed service was down, or under a
			// different model revision, stays available to exact recall only.
			continue
		}
		score := vector.Cosine(queryVector, note.embedding)
		if score < m.cfg.MinScore {
			continue
		}
		matches = append(matches, Match{Note: note, Score: score})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			return matches[i].UpdatedAt.After(matches[j].UpdatedAt)
		}
		return matches[i].Score > matches[j].Score
	})
	return limitMatches(matches, m.cfg.MaxResults), nil
}

// scanNote reads one row in the shared id/text/embedding/created/updated order.
func scanNote(rows *sql.Rows) (Note, error) {
	var (
		id        int64
		text      string
		embedding []byte
		createdMS int64
		updatedMS int64
	)
	if err := rows.Scan(&id, &text, &embedding, &createdMS, &updatedMS); err != nil {
		return Note{}, fmt.Errorf("note: read note: %w", err)
	}
	return Note{
		ID:        strconv.FormatInt(id, 10),
		Text:      text,
		CreatedAt: time.UnixMilli(createdMS),
		UpdatedAt: time.UnixMilli(updatedMS),
		embedding: decodeVector(embedding),
	}, nil
}

// embedText captures one note's vector at write time. A failure is logged and
// the note is still saved: recall can fall back to exact matches, and the next
// edit retries the vector.
func (m *Manager) embedText(text string) []float32 {
	if m.embed == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
	defer cancel()
	vectors, err := m.embed.Embed(ctx, []string{text})
	if err != nil {
		m.logger.Warn("note embedding failed", "error", err.Error())
		return nil
	}
	if err := validateVectors(vectors, 1); err != nil {
		m.logger.Warn("note embedding response invalid", "error", err.Error())
		return nil
	}
	return vectors[0]
}

// encodeVector stores a raw float32 vector little-endian. An empty vector
// becomes NULL, which is the "exact matches only" marker.
func encodeVector(vec []float32) []byte {
	if len(vec) == 0 {
		return nil
	}
	buf := make([]byte, len(vec)*4)
	for i, value := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(value))
	}
	return buf
}

// decodeVector reads the little-endian form; a malformed blob is ignored rather
// than allowed to panic a recall.
func decodeVector(raw []byte) []float32 {
	if len(raw) == 0 || len(raw)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(raw)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vec
}

// parseID accepts the decimal id the tools and the Dashboard use.
func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, &Error{Message: "That note id is not valid."}
	}
	return id, nil
}

// limitMatches bounds a result without changing its order.
func limitMatches(matches []Match, limit int) []Match {
	if limit <= 0 || len(matches) <= limit {
		return matches
	}
	return matches[:limit]
}

// validateVectors rejects a response the ranking cannot score: the wrong
// count, empty or inconsistent dimensions, or a non-finite value.
func validateVectors(vectors [][]float32, want int) error {
	if len(vectors) != want {
		return fmt.Errorf("note: got %d vectors for %d texts", len(vectors), want)
	}
	dims := 0
	for i, vec := range vectors {
		if len(vec) == 0 {
			return fmt.Errorf("note: vector %d is empty", i)
		}
		if dims == 0 {
			dims = len(vec)
		} else if len(vec) != dims {
			return fmt.Errorf("note: vector %d has %d dimensions, want %d", i, len(vec), dims)
		}
	}
	return nil
}
