package timer

import (
	"context"
	"fmt"
	"strings"
)

// SetAudio changes only a pending timer, atomically with its durable row.
func (m *Manager) SetAudio(ctx context.Context, id, audio string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return fmt.Errorf("timer is no longer pending")
	}
	audio = strings.TrimSpace(audio)
	if m.db != nil {
		if _, err := m.db.ExecContext(ctx, `UPDATE timers SET audio=? WHERE id=?`, audio, id); err != nil {
			return err
		}
	}
	e.timer.Audio = audio
	return nil
}
