package list

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

const storeTimeout = 5 * time.Second

// ErrUnavailable reports a list operation without a database.
var ErrUnavailable = errors.New("list: the list store is not available")

// ErrItemNotFound reports a spoken item no single row matched. It is never a
// forced guess: an ambiguous phrase returns this too.
var ErrItemNotFound = errors.New("list: item not found")

// Error is a spoken validation failure from the manager.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

// Item is one row on a fixed list.
type Item struct {
	ID        string    `json:"id"`
	List      string    `json:"list"`
	Text      string    `json:"text"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Options are the Manager's seams.
type Options struct {
	DB     *store.Store
	Names  *namematch.Matcher
	Now    func() time.Time
	Logger *slog.Logger
	Config Config
}

// Manager owns the persisted List items.
type Manager struct {
	db     *sql.DB
	names  *namematch.Matcher
	now    func() time.Time
	logger *slog.Logger
	cfg    Config
}

// NewManager builds a Manager. A nil store is allowed for registration and the
// conformance suite; every handler then fails with ErrUnavailable.
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
	return &Manager{db: db, names: opts.Names, now: opts.Now, logger: opts.Logger, cfg: cfg}
}

// Add stores one item unless the same open item is already there. Added is
// false for a duplicate, which is a real answer.
func (m *Manager) Add(ctx context.Context, key, text string) (Item, bool, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Item{}, false, &Error{Message: "What should I add?"}
	}
	items, err := m.Items(ctx, key)
	if err != nil {
		return Item{}, false, err
	}
	open := 0
	for _, item := range items {
		if item.Done {
			continue
		}
		open++
		if strings.EqualFold(item.Text, text) {
			return item, false, nil
		}
	}
	if open >= m.cfg.MaxItems {
		return Item{}, false, &Error{Message: "That list is full."}
	}
	now := m.now()
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`INSERT INTO list_items (list_key, text, done, created_at, updated_at) VALUES (?, ?, 0, ?, ?)`,
		key, text, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Item{}, false, fmt.Errorf("list: save item: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Item{}, false, fmt.Errorf("list: saved item id: %w", err)
	}
	m.logger.Info("list item added", "list", key, "item_id", id)
	return Item{ID: strconv.FormatInt(id, 10), List: key, Text: text, CreatedAt: now, UpdatedAt: now}, true, nil
}

// Items returns every row of one list, open first then done, oldest first. The
// Dashboard needs the done rows; the Fast-path read filters them out.
func (m *Manager) Items(ctx context.Context, key string) ([]Item, error) {
	if m.db == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, list_key, text, done, created_at, updated_at
		 FROM list_items WHERE list_key = ? ORDER BY done, id`,
		key)
	if err != nil {
		return nil, fmt.Errorf("list: read items: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list: read items: %w", err)
	}
	return items, nil
}

// Read returns the open items of one list, or both lists when key is empty.
func (m *Manager) Read(ctx context.Context, key string) ([]List, error) {
	keys := []string{KeyShopping, KeyTodo}
	if key != "" {
		keys = []string{key}
	}
	out := make([]List, 0, len(keys))
	for _, listKey := range keys {
		items, err := m.Items(ctx, listKey)
		if err != nil {
			return nil, err
		}
		open := make([]Item, 0, len(items))
		for _, item := range items {
			if !item.Done {
				open = append(open, item)
			}
		}
		out = append(out, List{Key: listKey, Title: Title(listKey), Items: open})
	}
	return out, nil
}

// UpdateItem replaces one row's text.
func (m *Manager) UpdateItem(ctx context.Context, key, id, text string) (Item, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Item{}, &Error{Message: "An item can't be empty."}
	}
	itemID, err := parseID(id)
	if err != nil {
		return Item{}, err
	}
	if m.db == nil {
		return Item{}, ErrUnavailable
	}
	now := m.now()
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`UPDATE list_items SET text = ?, updated_at = ? WHERE id = ? AND list_key = ?`,
		text, now.UnixMilli(), itemID, key)
	if err != nil {
		return Item{}, fmt.Errorf("list: update item: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Item{}, ErrItemNotFound
	}
	m.logger.Info("list item updated", "list", key, "item_id", itemID)
	return Item{ID: id, List: key, Text: text, UpdatedAt: now}, nil
}

// SetDone ticks or unticks one row.
func (m *Manager) SetDone(ctx context.Context, key, id string, done bool) (Item, error) {
	itemID, err := parseID(id)
	if err != nil {
		return Item{}, err
	}
	if m.db == nil {
		return Item{}, ErrUnavailable
	}
	now := m.now()
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`UPDATE list_items SET done = ?, updated_at = ? WHERE id = ? AND list_key = ?`,
		boolInt(done), now.UnixMilli(), itemID, key)
	if err != nil {
		return Item{}, fmt.Errorf("list: tick item: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return Item{}, ErrItemNotFound
	}
	items, err := m.Items(ctx, key)
	if err != nil {
		return Item{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return Item{}, ErrItemNotFound
}

// Remove deletes the one row a spoken item resolves to. The shared name matcher
// is the fallback after exact and substring matching.
func (m *Manager) Remove(ctx context.Context, key, spoken string) (Item, error) {
	spoken = strings.TrimSpace(spoken)
	if spoken == "" {
		return Item{}, ErrItemNotFound
	}
	items, err := m.Items(ctx, key)
	if err != nil {
		return Item{}, err
	}
	match, ok := m.resolveItem(ctx, spoken, items)
	if !ok {
		return Item{}, ErrItemNotFound
	}
	if err := m.RemoveByID(ctx, key, match.ID); err != nil {
		return Item{}, err
	}
	return match, nil
}

// resolveItem finds exactly one row for a spoken item. It never forces a guess:
// two equally plausible rows are no match.
func (m *Manager) resolveItem(ctx context.Context, spoken string, items []Item) (Item, bool) {
	var exact, contains []Item
	lower := strings.ToLower(spoken)
	for _, item := range items {
		switch {
		case strings.EqualFold(strings.TrimSpace(item.Text), spoken):
			exact = append(exact, item)
		case strings.Contains(strings.ToLower(item.Text), lower) || strings.Contains(lower, strings.ToLower(item.Text)):
			contains = append(contains, item)
		}
	}
	if len(exact) == 1 {
		return exact[0], true
	}
	if len(exact) > 1 {
		return Item{}, false
	}
	if len(contains) == 1 {
		return contains[0], true
	}
	if len(contains) > 1 {
		return Item{}, false
	}
	if m.names == nil {
		return Item{}, false
	}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	result := m.names.Match(ctx, spoken, texts)
	if !result.Matched() {
		return Item{}, false
	}
	for _, item := range items {
		if item.Text == result.Name {
			return item, true
		}
	}
	return Item{}, false
}

// RemoveByID deletes one row.
func (m *Manager) RemoveByID(ctx context.Context, key, id string) error {
	itemID, err := parseID(id)
	if err != nil {
		return err
	}
	if m.db == nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	result, err := m.db.ExecContext(ctx,
		`DELETE FROM list_items WHERE id = ? AND list_key = ?`, itemID, key)
	if err != nil {
		return fmt.Errorf("list: remove item: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrItemNotFound
	}
	m.logger.Info("list item removed", "list", key, "item_id", itemID)
	return nil
}

// All returns both fixed lists with every row, open and done. It is the
// Dashboard read seam.
func (m *Manager) All(ctx context.Context) ([]List, error) {
	out := make([]List, 0, 2)
	for _, key := range []string{KeyShopping, KeyTodo} {
		items, err := m.Items(ctx, key)
		if err != nil {
			return nil, err
		}
		out = append(out, List{Key: key, Title: Title(key), Items: items})
	}
	return out, nil
}

// scanItem reads one row in the shared id/list/text/done/created/updated order.
func scanItem(rows *sql.Rows) (Item, error) {
	var (
		id        int64
		key       string
		text      string
		done      int
		createdMS int64
		updatedMS int64
	)
	if err := rows.Scan(&id, &key, &text, &done, &createdMS, &updatedMS); err != nil {
		return Item{}, fmt.Errorf("list: read item: %w", err)
	}
	return Item{
		ID:        strconv.FormatInt(id, 10),
		List:      key,
		Text:      text,
		Done:      done != 0,
		CreatedAt: time.UnixMilli(createdMS),
		UpdatedAt: time.UnixMilli(updatedMS),
	}, nil
}

func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, &Error{Message: "That list item id is not valid."}
	}
	return id, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
