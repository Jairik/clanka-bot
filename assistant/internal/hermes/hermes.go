// Package hermes runs one explicitly requested model-only job at a time.
package hermes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	SecretName        = "HERMES_OPENCODE_GO_API_KEY"
	maxTaskBytes      = 4096
	maxResponseBytes  = 8192
	jobTimeout        = 50 * time.Second
	goToolsJobTimeout = 250 * time.Second
	keepJobs          = 50
)

// Manager owns the job lifecycle; the sidecar operates on one request at a time.
type Manager struct {
	ctx      context.Context
	db       *sql.DB
	run      func(context.Context, string, string) (string, error)
	key      func() string
	announce func(string)
	logger   *slog.Logger
	timeout  time.Duration

	mu     sync.Mutex
	closed bool
	active int64
	cancel context.CancelFunc
	wait   sync.WaitGroup
}

// New recovers abandoned jobs without replaying a request across a restart.
func New(ctx context.Context, db *sql.DB, endpoint, token string, key func() string, announce func(string), logger *slog.Logger) (*Manager, error) {
	if _, err := db.ExecContext(ctx, "UPDATE hermes_jobs SET state = 'interrupted' WHERE state = 'running'"); err != nil {
		return nil, err
	}
	// Match the sidecar flag and leave time for its worker cleanup.
	timeout := jobTimeout
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HERMES_GO_TOOLS"))) {
	case "1", "true", "yes", "on":
		timeout = goToolsJobTimeout
	}
	client := &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	m := &Manager{ctx: ctx, db: db, key: key, announce: announce, logger: logger, timeout: timeout}
	if len(token) >= 32 {
		m.run = func(ctx context.Context, task, key string) (string, error) {
			return call(ctx, client, endpoint, token, task, key)
		}
	}
	return m, nil
}

// Configured reports whether the sidecar and its provider key are configured.
func (m *Manager) Configured() bool {
	return m.run != nil && strings.TrimSpace(m.key()) != ""
}

// Close waits for a cancelled in-flight request to release the database.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wait.Wait()
}

// Handle receives a fresh explicit Hermes request, without prior turns or Tool data.
func (m *Manager) Handle(ctx context.Context, request string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "Hermes isn't accepting jobs right now."
	}
	command := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(request), "?.!,"))
	switch strings.ToLower(command) {
	case "":
		return "What should I ask Hermes?"
	case "status", "result":
		return m.latest(ctx, strings.EqualFold(command, "result"))
	case "cancel":
		if m.active == 0 {
			return "Hermes has no running job."
		}
		if err := m.finish(ctx, m.active, "cancelled", ""); err != nil {
			m.logger.Error("could not cancel Hermes job", "job_id", m.active, "error", err)
			return "I couldn't cancel the Hermes job."
		}
		m.cancel()
		return fmt.Sprintf("Hermes job %d was cancelled.", m.active)
	}

	if m.run == nil {
		return "Hermes is disabled."
	}
	key := strings.TrimSpace(m.key())
	if key == "" {
		return "Hermes needs its OpenCode Go key."
	}
	if len(request) > maxTaskBytes {
		return "That Hermes request is too long."
	}
	if m.active != 0 {
		return fmt.Sprintf("Hermes is already working on job %d.", m.active)
	}
	if m.ctx.Err() != nil || ctx.Err() != nil {
		return "Hermes isn't accepting jobs right now."
	}

	id, err := m.start(ctx)
	if err != nil {
		m.logger.Error("could not start Hermes job", "error", err)
		return "I couldn't start a Hermes job."
	}
	jobCtx, cancel := context.WithTimeout(m.ctx, m.timeout)
	m.active, m.cancel = id, cancel
	m.wait.Add(1)
	go m.execute(jobCtx, id, request, key)
	return fmt.Sprintf("Hermes is working on job %d.", id)
}

func (m *Manager) start(ctx context.Context) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, "INSERT INTO hermes_jobs (state) VALUES ('running') RETURNING id").Scan(&id)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hermes_jobs WHERE id <= ?", id-keepJobs); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (m *Manager) latest(ctx context.Context, wantResult bool) string {
	var id int64
	var state, result string
	err := m.db.QueryRowContext(ctx, "SELECT id, state, result FROM hermes_jobs ORDER BY id DESC LIMIT 1").Scan(&id, &state, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return "Hermes has no jobs yet."
	}
	if err != nil {
		m.logger.Error("could not read Hermes job", "error", err)
		return "I couldn't read the Hermes job."
	}
	if wantResult && state == "finished" {
		return result
	}
	return fmt.Sprintf("Hermes job %d is %s.", id, state)
}

func (m *Manager) finish(ctx context.Context, id int64, state, result string) error {
	_, err := m.db.ExecContext(ctx, "UPDATE hermes_jobs SET state = ?, result = ? WHERE id = ? AND state = 'running'", state, result, id)
	return err
}

func (m *Manager) execute(ctx context.Context, id int64, task, key string) {
	defer m.wait.Done()
	text, err := m.run(ctx, task, key)
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() {
		m.cancel()
		m.active, m.cancel = 0, nil
	}()

	state := "finished"
	if m.ctx.Err() != nil {
		state, text = "interrupted", ""
	} else if errors.Is(ctx.Err(), context.Canceled) {
		// Handle already persisted the cancellation before closing the request.
		return
	} else if err != nil {
		state, text = "failed", ""
		m.logger.Warn("Hermes job failed", "job_id", id)
	}
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 5*time.Second)
	defer cancel()
	if err := m.finish(updateCtx, id, state, text); err != nil {
		m.logger.Error("could not update Hermes job", "job_id", id, "error", err)
		return
	}
	if m.announce != nil && state != "interrupted" {
		m.announce("Hermes " + state + ".")
	}
}

func call(ctx context.Context, client *http.Client, endpoint, token, task, key string) (string, error) {
	body, err := json.Marshal(map[string]string{"task": task, "key": key})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Hermes sidecar returned HTTP %d", resp.StatusCode)
	}
	var reply struct{ Text string }
	limited := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	payload, err := io.ReadAll(limited)
	if err != nil || limited.N == 0 {
		return "", errors.New("invalid Hermes reply size")
	}
	if err := json.Unmarshal(payload, &reply); err != nil {
		return "", err
	}
	text := strings.Join(strings.Fields(reply.Text), " ")
	if text == "" {
		return "", errors.New("empty Hermes reply")
	}
	return text, nil
}
